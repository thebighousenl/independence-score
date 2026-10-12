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
}

func (f fakeStore) DayShares(ctx context.Context, day string) ([]store.DayShare, error) {
	return f.shares, nil
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
