// Package dashboard serves the one-day view (shares by company, by
// self-hosted category, and unclassified, always stating the unclassified
// percentage) plus a multi-day trend over the store's retained daily
// aggregates.
package dashboard

import (
	"context"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/thebighousenl/independence-score/internal/store"
)

// DataSource is the minimal read surface the dashboard needs from the
// store, kept as an interface so the handler is testable without SQLite.
type DataSource interface {
	DayShares(ctx context.Context, day string) ([]store.DayShare, error)
	Trend(ctx context.Context, startDay, endDay string) ([]store.TrendRow, error)
}

// defaultTrendWindowDays is how many calendar days the trend covers when
// the request does not ask for a different window.
const defaultTrendWindowDays = 14

// Handler serves the dashboard at "/": the one-day view plus the
// multi-day trend beneath it.
type Handler struct {
	Store DataSource
	// Now returns the current time; overridable in tests. Defaults to
	// time.Now.
	Now func() time.Time
}

type viewRow struct {
	Owner   string
	Count   int64
	Percent float64
}

type viewData struct {
	Day               string
	Total             int64
	Companies         []viewRow
	SelfHosted        []viewRow
	UnclassifiedCount int64
	UnclassifiedPct   float64
	Trend             trendViewData
}

// trendDay is one day's shares within the trend window, built with the
// exact same per-day logic as the one-day viewData above so the two views
// never disagree about what a day's unclassified share is.
type trendDay struct {
	Day               string
	Total             int64
	Companies         []viewRow
	SelfHosted        []viewRow
	UnclassifiedCount int64
	UnclassifiedPct   float64
}

// trendViewData is the multi-day trend. Days is only ever as long as the
// number of days that actually have aggregate data - never padded out to
// RequestedWindowDays - so a short history renders honestly as a short
// table instead of a chart implying a trend that is not there yet.
type trendViewData struct {
	RequestedWindowDays int
	Days                []trendDay
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	day := r.URL.Query().Get("day")
	if day == "" {
		day = h.now().UTC().Format("2006-01-02")
	}

	windowDays := defaultTrendWindowDays
	if v := r.URL.Query().Get("trend_days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			windowDays = n
		}
	}

	shares, err := h.Store.DayShares(r.Context(), day)
	if err != nil {
		http.Error(w, "loading shares: "+err.Error(), http.StatusInternalServerError)
		return
	}

	endDay, err := time.Parse("2006-01-02", day)
	if err != nil {
		http.Error(w, "invalid day: "+err.Error(), http.StatusBadRequest)
		return
	}
	startDay := endDay.AddDate(0, 0, -(windowDays - 1))

	trendRows, err := h.Store.Trend(r.Context(), startDay.Format("2006-01-02"), day)
	if err != nil {
		http.Error(w, "loading trend: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := buildView(day, shares)
	data.Trend = buildTrendView(windowDays, trendRows)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, data); err != nil {
		http.Error(w, "rendering dashboard: "+err.Error(), http.StatusInternalServerError)
	}
}

func buildView(day string, shares []store.DayShare) viewData {
	var total int64
	for _, s := range shares {
		total += s.Count
	}

	data := viewData{Day: day, Total: total}
	for _, s := range shares {
		pct := 0.0
		if total > 0 {
			pct = 100 * float64(s.Count) / float64(total)
		}
		switch s.Bucket {
		case store.BucketCompany:
			data.Companies = append(data.Companies, viewRow{s.Owner, s.Count, pct})
		case store.BucketSelfHosted:
			data.SelfHosted = append(data.SelfHosted, viewRow{s.Owner, s.Count, pct})
		case store.BucketUnclassified:
			data.UnclassifiedCount += s.Count
			data.UnclassifiedPct += pct
		}
	}

	sort.Slice(data.Companies, func(i, j int) bool { return data.Companies[i].Count > data.Companies[j].Count })
	sort.Slice(data.SelfHosted, func(i, j int) bool { return data.SelfHosted[i].Count > data.SelfHosted[j].Count })

	return data
}

// buildTrendView groups trend rows by day and reuses the same per-day
// share math as buildView, so the trend's unclassified percentage for a
// given day always agrees with what the one-day view would show for that
// same day. Only days present in rows are included: a day absent because
// its aggregate was never written renders as an absent row, not a zero
// one, so a short history looks like a short table rather than a flat
// line claiming more history than the store actually has.
func buildTrendView(requestedWindowDays int, rows []store.TrendRow) trendViewData {
	order := make([]string, 0)
	byDay := make(map[string][]store.DayShare)
	for _, r := range rows {
		if _, ok := byDay[r.Day]; !ok {
			order = append(order, r.Day)
		}
		byDay[r.Day] = append(byDay[r.Day], store.DayShare{Bucket: r.Bucket, Owner: r.Owner, Count: r.Count})
	}
	// Store.Trend already returns rows ordered by day, but sort here too
	// rather than trust every caller to preserve that: this is a cheap
	// guarantee to keep, not a hot path.
	sort.Strings(order)

	tv := trendViewData{RequestedWindowDays: requestedWindowDays}
	for _, day := range order {
		v := buildView(day, byDay[day])
		tv.Days = append(tv.Days, trendDay{
			Day:               v.Day,
			Total:             v.Total,
			Companies:         v.Companies,
			SelfHosted:        v.SelfHosted,
			UnclassifiedCount: v.UnclassifiedCount,
			UnclassifiedPct:   v.UnclassifiedPct,
		})
	}
	return tv
}

var pageTemplate = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>Independence Score — {{.Day}}</title></head>
<body>
<h1>Independence Score — {{.Day}}</h1>
<p>Total queries classified: {{.Total}}</p>
<p><strong>Unclassified: {{printf "%.1f" .UnclassifiedPct}}%</strong> ({{.UnclassifiedCount}} queries) — this share is not folded into either bucket below.</p>

<h2>By company</h2>
<table border="1" cellpadding="4">
<tr><th>Company</th><th>Queries</th><th>Share</th></tr>
{{range .Companies}}<tr><td>{{.Owner}}</td><td>{{.Count}}</td><td>{{printf "%.1f" .Percent}}%</td></tr>
{{else}}<tr><td colspan="3">No known-company traffic today.</td></tr>
{{end}}
</table>

<h2>By self-hosted category</h2>
<table border="1" cellpadding="4">
<tr><th>Software</th><th>Queries</th><th>Share</th></tr>
{{range .SelfHosted}}<tr><td>{{.Owner}}</td><td>{{.Count}}</td><td>{{printf "%.1f" .Percent}}%</td></tr>
{{else}}<tr><td colspan="3">No known self-hosted traffic today.</td></tr>
{{end}}
</table>

<h2>Trend</h2>
<p>{{len .Trend.Days}} of the {{.Trend.RequestedWindowDays}} requested days have data.
{{if lt (len .Trend.Days) .Trend.RequestedWindowDays}}This instance does not have a full window of history yet, so the table below is shorter than the window requested — that is not a flat trend, it is all the history there is so far.{{end}}</p>
<table border="1" cellpadding="4">
<tr><th>Day</th><th>Total</th><th>Unclassified</th><th>By company</th><th>By self-hosted</th></tr>
{{range .Trend.Days}}<tr>
<td>{{.Day}}</td>
<td>{{.Total}}</td>
<td>{{printf "%.1f" .UnclassifiedPct}}% ({{.UnclassifiedCount}})</td>
<td>{{range .Companies}}{{.Owner}}: {{printf "%.1f" .Percent}}%<br>{{else}}—{{end}}</td>
<td>{{range .SelfHosted}}{{.Owner}}: {{printf "%.1f" .Percent}}%<br>{{else}}—{{end}}</td>
</tr>
{{else}}<tr><td colspan="5">No data in this window yet.</td></tr>
{{end}}
</table>
</body>
</html>
`))
