package store

import (
	"context"
	"fmt"
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

func TestStatsReportsDroppedCountWhenBufferOverflows(t *testing.T) {
	s := openTestStore(t)

	if dropped := s.Stats(); dropped != 0 {
		t.Fatalf("dropped = %d before any overflow, want 0", dropped)
	}

	// Enough events, fast enough, to outrun the background writer and
	// force the internal buffer to fill. This must be visible in Stats:
	// a dropped query must never be invisible.
	for i := 0; i < queueSize*4; i++ {
		s.LogQuery(QueryEvent{Hostname: "flood.example", Qtype: 1, At: time.Now()})
	}

	if dropped := s.Stats(); dropped == 0 {
		t.Fatal("dropped = 0 after flooding past queue capacity, want > 0")
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

func TestRegisteredDomainCollapsesSubdomainsAndHandlesMultiLabelSuffixes(t *testing.T) {
	for _, tc := range []struct {
		hostname string
		want     string
	}{
		{"www.example.com", "example.com"},
		{"a.b.c.example.com", "example.com"},
		{"example.com", "example.com"},
		// co.uk is a multi-label public suffix; a naive "last two labels"
		// rule would wrongly group every co.uk site under "co.uk".
		{"shop.example.co.uk", "example.co.uk"},
		// A bare public suffix has no registered domain to collapse to;
		// grouping must fall back to the hostname itself rather than error.
		{"co.uk", "co.uk"},
		{"EXAMPLE.COM.", "example.com"},
	} {
		if got := registeredDomain(tc.hostname); got != tc.want {
			t.Errorf("registeredDomain(%q) = %q, want %q", tc.hostname, got, tc.want)
		}
	}
}

func TestRollUpGroupsUnclassifiedTrafficByRegisteredDomain(t *testing.T) {
	s := openTestStore(t)

	day := time.Now().UTC()
	s.LogQuery(QueryEvent{Hostname: "a.unknown-one.example", Qtype: 1, At: day})
	s.LogQuery(QueryEvent{Hostname: "b.unknown-one.example", Qtype: 1, At: day})
	s.LogQuery(QueryEvent{Hostname: "unknown-two.example", Qtype: 1, At: day})
	s.LogQuery(QueryEvent{Hostname: "known.example", Qtype: 1, At: day})
	waitForRawCount(t, s, 4)

	classify := func(hostname string) (BucketType, string) {
		if hostname == "known.example" {
			return BucketCompany, "KnownCo"
		}
		return BucketUnclassified, ""
	}

	ctx := context.Background()
	if err := s.RollUp(ctx, classify); err != nil {
		t.Fatalf("RollUp: %v", err)
	}

	got, err := s.UnclassifiedDomainsHistory(ctx, day.Format("2006-01-02"))
	if err != nil {
		t.Fatalf("UnclassifiedDomainsHistory: %v", err)
	}

	counts := map[string]int64{}
	for _, dc := range got {
		counts[dc.Domain] = dc.Count
	}
	if counts["unknown-one.example"] != 2 {
		t.Errorf("unknown-one.example = %d, want 2 (two subdomains collapsed)", counts["unknown-one.example"])
	}
	if counts["unknown-two.example"] != 1 {
		t.Errorf("unknown-two.example = %d, want 1", counts["unknown-two.example"])
	}
	if _, known := counts["known.example"]; known {
		t.Errorf("known.example must not appear in unclassified domain history, got %+v", got)
	}
}

func TestRollUpCapsUnclassifiedDomainsPerDay(t *testing.T) {
	s := openTestStore(t)

	day := time.Now().UTC()
	// More distinct unclassified domains than unclassifiedDomainsCap,
	// each with a different count so the top N are unambiguous.
	total := unclassifiedDomainsCap + 10
	for i := 0; i < total; i++ {
		host := fmt.Sprintf("site-%03d.example", i)
		// Higher index => more queries, so the lowest-index (lowest
		// count) sites are the ones that must be trimmed away.
		for n := 0; n < i+1; n++ {
			s.LogQuery(QueryEvent{Hostname: host, Qtype: 1, At: day})
		}
	}
	waitForRawCount(t, s, total*(total+1)/2)

	classify := func(hostname string) (BucketType, string) { return BucketUnclassified, "" }
	ctx := context.Background()
	if err := s.RollUp(ctx, classify); err != nil {
		t.Fatalf("RollUp: %v", err)
	}

	got, err := s.UnclassifiedDomainsHistory(ctx, day.Format("2006-01-02"))
	if err != nil {
		t.Fatalf("UnclassifiedDomainsHistory: %v", err)
	}
	if len(got) != unclassifiedDomainsCap {
		t.Fatalf("got %d domains, want capped at %d", len(got), unclassifiedDomainsCap)
	}

	// The highest-count domain (site-<total-1>) must have survived the cap.
	top := fmt.Sprintf("site-%03d.example", total-1)
	found := false
	for _, dc := range got {
		if dc.Domain == top {
			found = true
		}
	}
	if !found {
		t.Errorf("expected top domain %s to survive the cap, got %+v", top, got)
	}

	// The lowest-count domain (site-000) must have been trimmed away.
	bottom := "site-000.example"
	for _, dc := range got {
		if dc.Domain == bottom {
			t.Errorf("expected lowest-count domain %s to be trimmed by the cap, got %+v", bottom, got)
		}
	}
}

func TestUnclassifiedDomainsLiveReadsRawQueriesForTheDay(t *testing.T) {
	s := openTestStore(t)

	day := time.Now().UTC()
	s.LogQuery(QueryEvent{Hostname: "mystery.example", Qtype: 1, At: day})
	s.LogQuery(QueryEvent{Hostname: "www.mystery.example", Qtype: 1, At: day})
	s.LogQuery(QueryEvent{Hostname: "known.example", Qtype: 1, At: day})
	waitForRawCount(t, s, 3)

	classify := func(hostname string) (BucketType, string) {
		if hostname == "known.example" {
			return BucketCompany, "KnownCo"
		}
		return BucketUnclassified, ""
	}

	// Deliberately do not RollUp: UnclassifiedDomainsLive reads
	// raw_queries directly, independent of the roll-up cursor.
	got, err := s.UnclassifiedDomainsLive(context.Background(), day.Format("2006-01-02"), classify)
	if err != nil {
		t.Fatalf("UnclassifiedDomainsLive: %v", err)
	}
	if len(got) != 1 || got[0].Domain != "mystery.example" || got[0].Count != 2 {
		t.Fatalf("got %+v, want a single mystery.example row with count 2", got)
	}
}

func TestUnclassifiedDomainsLiveIsEmptyOnceRawRowsArePruned(t *testing.T) {
	s := openTestStore(t)

	old := time.Now().Add(-72 * time.Hour)
	s.LogQuery(QueryEvent{Hostname: "old-mystery.example", Qtype: 1, At: old})
	waitForRawCount(t, s, 1)

	ctx := context.Background()
	classify := func(hostname string) (BucketType, string) { return BucketUnclassified, "" }
	if err := s.Prune(ctx, time.Now()); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	got, err := s.UnclassifiedDomainsLive(ctx, old.UTC().Format("2006-01-02"), classify)
	if err != nil {
		t.Fatalf("UnclassifiedDomainsLive: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want empty once raw rows are pruned (caller must check RawRetentionCutoff, not infer from this)", got)
	}
}
