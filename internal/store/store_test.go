package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.sqlite"), 48*time.Hour)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// waitForRawCount polls until the background writer has flushed at least
// n rows, or fails the test after a timeout. The writer flushes on a
// 500ms ticker or when a batch fills, so logging is asynchronous by
// design.
func waitForRawCount(t *testing.T, s *Store, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_queries`).Scan(&count); err != nil {
			t.Fatalf("counting raw_queries: %v", err)
		}
		if count >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d raw rows to be flushed", n)
}

func TestLogQueryIsAsyncAndNonBlocking(t *testing.T) {
	s := openTestStore(t)

	start := time.Now()
	s.LogQuery(QueryEvent{Hostname: "example.com", Qtype: 1, At: time.Now()})
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("LogQuery took %v, want effectively instant (non-blocking)", elapsed)
	}

	waitForRawCount(t, s, 1)
}

func TestLogQueryNeverBlocksWhenBufferIsFull(t *testing.T) {
	s := openTestStore(t)

	// Fill well past the internal queue capacity; none of these calls may
	// block, and the store must still be alive afterward.
	start := time.Now()
	for i := 0; i < queueSize*4; i++ {
		s.LogQuery(QueryEvent{Hostname: "flood.example", Qtype: 1, At: time.Now()})
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("flooding LogQuery took %v, want it to never block", elapsed)
	}
}

func TestRollUpClassifiesAndAggregates(t *testing.T) {
	s := openTestStore(t)

	day := time.Now().UTC()
	s.LogQuery(QueryEvent{Hostname: "graph.facebook.com", Qtype: 1, At: day})
	s.LogQuery(QueryEvent{Hostname: "graph.facebook.com", Qtype: 1, At: day})
	s.LogQuery(QueryEvent{Hostname: "mynextcloud.example", Qtype: 1, At: day})
	s.LogQuery(QueryEvent{Hostname: "totally-unknown.example", Qtype: 1, At: day})
	waitForRawCount(t, s, 4)

	classify := func(hostname string) (BucketType, string) {
		switch hostname {
		case "graph.facebook.com":
			return BucketCompany, "Meta"
		case "mynextcloud.example":
			return BucketSelfHosted, "Nextcloud"
		default:
			return BucketUnclassified, ""
		}
	}

	ctx := context.Background()
	if err := s.RollUp(ctx, classify); err != nil {
		t.Fatalf("RollUp: %v", err)
	}

	shares, err := s.DayShares(ctx, day.Format("2006-01-02"))
	if err != nil {
		t.Fatalf("DayShares: %v", err)
	}

	got := map[string]int64{}
	for _, sh := range shares {
		got[string(sh.Bucket)+"|"+sh.Owner] = sh.Count
	}

	if got["company|Meta"] != 2 {
		t.Errorf("company|Meta = %d, want 2", got["company|Meta"])
	}
	if got["self-hosted|Nextcloud"] != 1 {
		t.Errorf("self-hosted|Nextcloud = %d, want 1", got["self-hosted|Nextcloud"])
	}
	if got["unclassified|"] != 1 {
		t.Errorf("unclassified| = %d, want 1", got["unclassified|"])
	}

	// A second roll-up with no new rows must not double-count.
	if err := s.RollUp(ctx, classify); err != nil {
		t.Fatalf("second RollUp: %v", err)
	}
	shares, err = s.DayShares(ctx, day.Format("2006-01-02"))
	if err != nil {
		t.Fatalf("DayShares after second RollUp: %v", err)
	}
	for _, sh := range shares {
		if sh.Bucket == BucketCompany && sh.Owner == "Meta" && sh.Count != 2 {
			t.Errorf("after second RollUp, company|Meta = %d, want still 2 (no double count)", sh.Count)
		}
	}
}

func TestPruneRemovesOldRawRowsButKeepsAggregates(t *testing.T) {
	s := openTestStore(t)

	old := time.Now().Add(-72 * time.Hour)
	s.LogQuery(QueryEvent{Hostname: "old.example", Qtype: 1, At: old})
	waitForRawCount(t, s, 1)

	ctx := context.Background()
	classify := func(hostname string) (BucketType, string) { return BucketUnclassified, "" }
	if err := s.RollUp(ctx, classify); err != nil {
		t.Fatalf("RollUp: %v", err)
	}

	if err := s.Prune(ctx, time.Now()); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	var rawCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_queries`).Scan(&rawCount); err != nil {
		t.Fatalf("counting raw_queries: %v", err)
	}
	if rawCount != 0 {
		t.Errorf("raw_queries count = %d, want 0 after pruning past retention", rawCount)
	}

	shares, err := s.DayShares(ctx, old.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("DayShares: %v", err)
	}
	if len(shares) == 0 {
		t.Error("expected daily aggregate to survive pruning of raw rows")
	}
}

func TestTrendReturnsOnlyDaysWithAggregates(t *testing.T) {
	s := openTestStore(t)

	day1 := time.Now().Add(-48 * time.Hour).UTC()
	day2 := time.Now().Add(-24 * time.Hour).UTC()
	// day3 (today) deliberately gets no queries at all, to prove it is
	// simply absent from Trend rather than showing up as a zero row.

	s.LogQuery(QueryEvent{Hostname: "graph.facebook.com", Qtype: 1, At: day1})
	s.LogQuery(QueryEvent{Hostname: "graph.facebook.com", Qtype: 1, At: day2})
	s.LogQuery(QueryEvent{Hostname: "graph.facebook.com", Qtype: 1, At: day2})
	waitForRawCount(t, s, 3)

	classify := func(hostname string) (BucketType, string) { return BucketCompany, "Meta" }
	ctx := context.Background()
	if err := s.RollUp(ctx, classify); err != nil {
		t.Fatalf("RollUp: %v", err)
	}

	start := day1.Add(-24 * time.Hour).Format("2006-01-02")
	end := time.Now().UTC().Format("2006-01-02")
	rows, err := s.Trend(ctx, start, end)
	if err != nil {
		t.Fatalf("Trend: %v", err)
	}

	byDay := map[string]int64{}
	for _, r := range rows {
		byDay[r.Day] += r.Count
	}
	if got := byDay[day1.Format("2006-01-02")]; got != 1 {
		t.Errorf("day1 count = %d, want 1", got)
	}
	if got := byDay[day2.Format("2006-01-02")]; got != 2 {
		t.Errorf("day2 count = %d, want 2", got)
	}
	if _, ok := byDay[time.Now().UTC().Format("2006-01-02")]; ok {
		t.Error("today has no traffic and must be absent from Trend, not present as a zero row")
	}

	// Trend rows must come back in chronological order.
	for i := 1; i < len(rows); i++ {
		if rows[i].Day < rows[i-1].Day {
			t.Fatalf("Trend rows out of order: %q before %q", rows[i-1].Day, rows[i].Day)
		}
	}
}

func TestTrendSurvivesPruningOfRawRows(t *testing.T) {
	s := openTestStore(t)

	old := time.Now().Add(-72 * time.Hour)
	s.LogQuery(QueryEvent{Hostname: "old.example", Qtype: 1, At: old})
	waitForRawCount(t, s, 1)

	ctx := context.Background()
	classify := func(hostname string) (BucketType, string) { return BucketUnclassified, "" }
	if err := s.RollUp(ctx, classify); err != nil {
		t.Fatalf("RollUp: %v", err)
	}
	if err := s.Prune(ctx, time.Now()); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	start := old.Add(-24 * time.Hour).UTC().Format("2006-01-02")
	end := time.Now().UTC().Format("2006-01-02")
	rows, err := s.Trend(ctx, start, end)
	if err != nil {
		t.Fatalf("Trend: %v", err)
	}

	var found bool
	for _, r := range rows {
		if r.Day == old.UTC().Format("2006-01-02") {
			found = true
		}
	}
	if !found {
		t.Error("expected the pruned day's aggregate to still appear in Trend")
	}
}
