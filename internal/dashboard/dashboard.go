// Package dashboard serves the M1 one-day view: shares by company, by
// self-hosted category, and unclassified, for a single day, always
// stating the unclassified percentage.
package dashboard

import (
	"context"
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/thebighousenl/independence-score/internal/store"
)

// DayShares is the minimal read surface the dashboard needs from the
// store, kept as an interface so the handler is testable without SQLite.
type DayShares interface {
	DayShares(ctx context.Context, day string) ([]store.DayShare, error)
}

// Handler serves the one-day dashboard at "/".
type Handler struct {
	Store DayShares
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

	shares, err := h.Store.DayShares(r.Context(), day)
	if err != nil {
		http.Error(w, "loading shares: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := buildView(day, shares)

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
</body>
</html>
`))
