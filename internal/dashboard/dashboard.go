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
	UnclassifiedDomainsLive(ctx context.Context, day string, classify store.ClassifyFunc) ([]store.DomainCount, error)
	UnclassifiedDomainsHistory(ctx context.Context, day string) ([]store.DomainCount, error)
	RawRetentionCutoff(now time.Time) time.Time
}

// defaultTrendWindowDays is how many calendar days the trend covers when
// the request does not ask for a different window.
const defaultTrendWindowDays = 14

// Handler serves the dashboard at "/": the one-day view plus the
// multi-day trend beneath it.
type Handler struct {
	Store DataSource
	// Classify buckets a hostname the same way the roll-up loop does. It
	// is only used for the top-unclassified-domains view's live read
	// path (a day still inside raw retention); days outside that window
	// read the pre-classified unclassified_domains roll-up instead.
	Classify store.ClassifyFunc
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
	Day                 string
	Total               int64
	Companies           []viewRow
	SelfHosted          []viewRow
	UnclassifiedCount   int64
	UnclassifiedPct     float64
	Trend               trendViewData
	UnclassifiedDomains unclassifiedDomainsView
}

// unclassifiedDomainsView is the top-unclassified-domains evidence for
// the chosen day: which registered domains made up the unclassified
// share, so a human can tell "a few missing companies" from "a long tail
// of ordinary sites" without guessing from the headline percentage
// alone.
//
// Source names where Domains came from, because the two sources make
// different honesty guarantees:
//   - "live": read straight from raw_queries, which is still fully
//     within the retention window for this day. Exact, not capped by
//     the roll-up's top-N budget.
//   - "history": read from the unclassified_domains roll-up, which
//     outlives the 48h raw-row prune but is capped at the top N domains
//     recorded at roll-up time.
//   - "none": the day is outside the retention window and no roll-up
//     snapshot exists for it either. Domains is empty here, and that
//     emptiness must not be read as "nothing was unclassified" - Note
//     says so.
type unclassifiedDomainsView struct {
	Domains []store.DomainCount
	Source  string
	Note    string
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
//
// A missing day inside the window is one of two different facts, and the
// page must not conflate them:
//
//   - ShortHistoryDays counts missing days before the first day that has
//     data - the instance simply has not been running long enough yet.
//   - GapDays names missing days after that first day of data - a hole in
//     an otherwise-populated window (container down, or genuinely no
//     traffic; the aggregate alone cannot tell which, so this does not
//     guess).
type trendViewData struct {
	RequestedWindowDays int
	Days                []trendDay
	ShortHistoryDays    int
	GapDays             []string
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
	data.Trend = buildTrendView(windowDays, startDay, endDay, trendRows)

	domainsView, err := h.unclassifiedDomainsView(r.Context(), day, endDay)
	if err != nil {
		http.Error(w, "loading unclassified domains: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data.UnclassifiedDomains = domainsView

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

// unclassifiedDomainsView picks between the live and history read paths
// for day's top-unclassified-domains evidence, and states plainly when
// neither has anything to show rather than letting an empty Domains
// slice be read as "nothing was unclassified".
//
// dayStart is day parsed as a UTC midnight instant. The day is treated as
// fully covered by raw retention - and so read live, exactly - only when
// its entire span is still at or after the retention cutoff. Otherwise
// some or all of its raw rows may already be pruned, so the capped
// history roll-up is used instead; that roll-up is written on every
// RollUp regardless of retention, so it also covers "today" up to the
// last roll-up tick even when the live path is used for the headline
// view.
func (h *Handler) unclassifiedDomainsView(ctx context.Context, day string, dayStart time.Time) (unclassifiedDomainsView, error) {
	cutoff := h.Store.RawRetentionCutoff(h.now())

	if !dayStart.Before(cutoff) {
		domains, err := h.Store.UnclassifiedDomainsLive(ctx, day, h.Classify)
		if err != nil {
			return unclassifiedDomainsView{}, err
		}
		return unclassifiedDomainsView{Domains: domains, Source: "live"}, nil
	}

	domains, err := h.Store.UnclassifiedDomainsHistory(ctx, day)
	if err != nil {
		return unclassifiedDomainsView{}, err
	}
	if len(domains) == 0 {
		return unclassifiedDomainsView{
			Source: "none",
			Note:   "This day is outside the raw-query retention window and no per-domain roll-up was recorded for it. An empty list here does not mean nothing was unclassified — the evidence was not kept, or this day predates this view.",
		}, nil
	}
	return unclassifiedDomainsView{
		Domains: domains,
		Source:  "history",
		Note:    "This day is outside the raw-query retention window. Showing the roll-up's stored snapshot, capped to its top domains at roll-up time — not necessarily every unclassified domain that day.",
	}, nil
}

// buildTrendView groups trend rows by day and reuses the same per-day
// share math as buildView, so the trend's unclassified percentage for a
// given day always agrees with what the one-day view would show for that
// same day. Only days present in rows are included: a day absent because
// its aggregate was never written renders as an absent row, not a zero
// one, so a short history looks like a short table rather than a flat
// line claiming more history than the store actually has.
//
// windowStart and windowEnd are walked day by day to classify every
// missing day as either short history (before the first day with data) or
// a gap (after it) - see trendViewData's doc comment. Nothing is padded
// into Days itself; ShortHistoryDays and GapDays are reported separately
// so the template can say exactly which fact is true.
func buildTrendView(requestedWindowDays int, windowStart, windowEnd time.Time, rows []store.TrendRow) trendViewData {
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

	present := make(map[string]bool, len(order))
	for _, d := range order {
		present[d] = true
	}
	sawData := false
	for d := windowStart; !d.After(windowEnd); d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		if present[ds] {
			sawData = true
			continue
		}
		if !sawData {
			tv.ShortHistoryDays++
		} else {
			tv.GapDays = append(tv.GapDays, ds)
		}
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

<h2>Top unclassified domains</h2>
{{if eq .UnclassifiedDomains.Source "none"}}
<p><em>{{.UnclassifiedDomains.Note}}</em></p>
{{else}}
{{if .UnclassifiedDomains.Note}}<p><em>{{.UnclassifiedDomains.Note}}</em></p>{{end}}
<table border="1" cellpadding="4">
<tr><th>Registered domain</th><th>Queries</th></tr>
{{range .UnclassifiedDomains.Domains}}<tr><td>{{.Domain}}</td><td>{{.Count}}</td></tr>
{{else}}<tr><td colspan="2">No unclassified domains recorded for this day.</td></tr>
{{end}}
</table>
{{end}}

<h2>Trend</h2>
<p>{{len .Trend.Days}} of the {{.Trend.RequestedWindowDays}} requested days have data.
{{if .Trend.ShortHistoryDays}} This instance does not have a full window of history yet, so the table below is shorter than the window requested — that is not a flat trend, it is all the history there is so far.{{end}}
{{if .Trend.GapDays}} This window also has a gap: {{len .Trend.GapDays}} day(s) inside it have no stored aggregate ({{range $i, $d := .Trend.GapDays}}{{if $i}}, {{end}}{{$d}}{{end}}) — the container may have been down that day, or there was genuinely no traffic; the aggregate alone cannot tell which, so this does not guess.{{end}}</p>
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
