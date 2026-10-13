package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thebighousenl/independence-score/internal/store"
)

type fakeStore struct {
	shares []store.DayShare
	trend  []store.TrendRow
}

func (f fakeStore) DayShares(ctx context.Context, day string) ([]store.DayShare, error) {
	return f.shares, nil
}

func (f fakeStore) Trend(ctx context.Context, startDay, endDay string) ([]store.TrendRow, error) {
	return f.trend, nil
}

func TestDashboardShowsNonZeroCompanySharesAndUnclassifiedPercent(t *testing.T) {
	h := &Handler{Store: fakeStore{shares: []store.DayShare{
		{Bucket: store.BucketCompany, Owner: "Meta", Count: 60},
		{Bucket: store.BucketCompany, Owner: "Google", Count: 20},
		{Bucket: store.BucketSelfHosted, Owner: "Nextcloud", Count: 10},
		{Bucket: store.BucketUnclassified, Owner: "", Count: 10},
	}}}

	req := httptest.NewRequest(http.MethodGet, "/?day=2026-10-12", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{"Meta", "60", "Google", "Nextcloud", "Unclassified: 10.0%"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard body missing %q\nfull body:\n%s", want, body)
		}
	}
}

func TestDashboardHandlesEmptyDayWithoutError(t *testing.T) {
	h := &Handler{Store: fakeStore{shares: nil}}

	req := httptest.NewRequest(http.MethodGet, "/?day=2026-01-01", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Unclassified: 0.0%") {
		t.Errorf("expected a stated 0%% unclassified share on an empty day, got:\n%s", rec.Body.String())
	}
}

func TestDashboardShowsMultiDayTrendWithPerDayUnclassifiedPercent(t *testing.T) {
	h := &Handler{Store: fakeStore{trend: []store.TrendRow{
		{Day: "2026-10-10", Bucket: store.BucketCompany, Owner: "Meta", Count: 80},
		{Day: "2026-10-10", Bucket: store.BucketUnclassified, Owner: "", Count: 20},
		{Day: "2026-10-11", Bucket: store.BucketCompany, Owner: "Meta", Count: 50},
		{Day: "2026-10-11", Bucket: store.BucketUnclassified, Owner: "", Count: 50},
	}}}

	req := httptest.NewRequest(http.MethodGet, "/?day=2026-10-11", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{"2026-10-10", "2026-10-11", "20.0% (20)", "50.0% (50)"} {
		if !strings.Contains(body, want) {
			t.Errorf("trend view missing %q\nfull body:\n%s", want, body)
		}
	}
}

func TestDashboardTrendRendersShortHistoryHonestly(t *testing.T) {
	// Only one day of trend data exists, even though the default window
	// requests many more. The page must say so plainly rather than
	// rendering something that implies a longer trend than the store
	// actually has.
	h := &Handler{Store: fakeStore{trend: []store.TrendRow{
		{Day: "2026-10-12", Bucket: store.BucketCompany, Owner: "Meta", Count: 10},
	}}}

	req := httptest.NewRequest(http.MethodGet, "/?day=2026-10-12", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "1 of the 14 requested days have data") {
		t.Errorf("expected an honest day count, got:\n%s", body)
	}
	if !strings.Contains(body, "does not have a full window of history yet") {
		t.Errorf("expected the short-history caveat, got:\n%s", body)
	}
}

func TestDashboardHonoursTrendDaysQueryParam(t *testing.T) {
	h := &Handler{Store: fakeStore{trend: nil}}

	req := httptest.NewRequest(http.MethodGet, "/?day=2026-10-12&trend_days=7", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "0 of the 7 requested days have data") {
		t.Errorf("expected the requested window to be honoured, got:\n%s", rec.Body.String())
	}
}

func TestBuildTrendViewGroupsRowsByDayAndKeepsChronologicalOrder(t *testing.T) {
	rows := []store.TrendRow{
		{Day: "2026-10-10", Bucket: store.BucketCompany, Owner: "Meta", Count: 5},
		{Day: "2026-10-12", Bucket: store.BucketUnclassified, Owner: "", Count: 3},
		{Day: "2026-10-11", Bucket: store.BucketSelfHosted, Owner: "Nextcloud", Count: 2},
	}

	tv := buildTrendView(14, rows)

	if len(tv.Days) != 3 {
		t.Fatalf("got %d days, want 3", len(tv.Days))
	}
	wantOrder := []string{"2026-10-10", "2026-10-11", "2026-10-12"}
	for i, want := range wantOrder {
		if tv.Days[i].Day != want {
			t.Fatalf("day[%d] = %q, want %q (trend must be chronological)", i, tv.Days[i].Day, want)
		}
	}
	if tv.Days[2].UnclassifiedCount != 3 {
		t.Fatalf("2026-10-12 UnclassifiedCount = %d, want 3", tv.Days[2].UnclassifiedCount)
	}
}

func TestBuildViewNeverFoldsUnclassifiedIntoOtherBuckets(t *testing.T) {
	shares := []store.DayShare{
		{Bucket: store.BucketCompany, Owner: "Meta", Count: 50},
		{Bucket: store.BucketUnclassified, Owner: "", Count: 50},
	}
	v := buildView("2026-10-12", shares)

	if len(v.Companies) != 1 || v.Companies[0].Owner != "Meta" || v.Companies[0].Count != 50 {
		t.Fatalf("company bucket corrupted: %+v", v.Companies)
	}
	if v.UnclassifiedCount != 50 {
		t.Fatalf("UnclassifiedCount = %d, want 50", v.UnclassifiedCount)
	}
	if v.UnclassifiedPct != 50.0 {
		t.Fatalf("UnclassifiedPct = %v, want 50.0", v.UnclassifiedPct)
	}
}
