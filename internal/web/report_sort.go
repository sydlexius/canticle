package web

import (
	"net/url"

	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/tablesort"
	"github.com/sydlexius/canticle/web/templates"
)

// Sort namespaces of the Reports tables (#1260); the names are
// tablesort.Namespaces, which the request-log validators accept.
const (
	nsRecentOutcomes = "ro"
	nsInstrumentals  = "in"
	nsReviewQueue    = "rq"
)

// reportSortSpecs maps each namespace to its table's spec, so a sort link can
// carry every OTHER table's current (validated) sort.
var reportSortSpecs = map[string]tablesort.Spec{
	nsRecentOutcomes: reports.RecentOutcomesSpec,
	nsInstrumentals:  reports.InstrumentalSpec,
	nsReviewQueue:    reports.ReviewQueueSpec,
}

// sortCol is one header: its label and sort key ("" = not sortable).
type sortCol struct{ label, key string }

// Column sets of the Reports tables, in display order (Artist, Album, Title
// first). ID, File, Detail, Lyrics are not sortable.
var (
	recentOutcomeCols = []sortCol{
		{"Artist", tablesort.KeyArtist}, {"Album", tablesort.KeyAlbum}, {"Title", tablesort.KeyTitle},
		{"Result", tablesort.KeyResult}, {"Detail", ""}, {"Source", tablesort.KeySource},
		{"Completed", tablesort.KeyCompleted},
	}
	instrumentalCols = []sortCol{
		{"Artist", tablesort.KeyArtist}, {"Album", tablesort.KeyAlbum}, {"Title", tablesort.KeyTitle},
		{"ID", ""}, {"File", ""}, {"Detect requested", tablesort.KeyDetect},
	}
	reviewQueueCols = []sortCol{
		{"Artist", tablesort.KeyArtist}, {"Album", tablesort.KeyAlbum}, {"Title", tablesort.KeyTitle},
		{"Outcome", tablesort.KeyOutcome}, {"Overrun (s)", tablesort.KeyOverrun}, {"Ratio", tablesort.KeyRatio},
		{"Evaluated", tablesort.KeyEvaluated}, {"Lyrics", ""},
	}
)

// sortHeaders shapes a header row for the shared SortHeader component: each
// sortable header links to base with keep plus the order a click requests, under
// namespace ns.
func sortHeaders(base string, cols []sortCol, spec tablesort.Spec, active tablesort.Order, ns string, keep url.Values) []templates.SortHeaderView {
	out := make([]templates.SortHeaderView, 0, len(cols))
	for _, c := range cols {
		h := templates.SortHeaderView{Label: c.label}
		if c.key != "" {
			h.Href = tablesort.HeaderHrefNS(base, keep, spec.Toggle(active, c.key), ns)
			h.Aria = tablesort.AriaSort(active, c.key)
		}
		out = append(out, h)
	}
	return out
}

// otherSorts is the validated sort/dir of every namespace but ns in q: the
// params a table's header links must carry so sorting it never resets another
// table on the same page. An invalid value is dropped, never reflected.
func otherSorts(q url.Values, ns string) url.Values {
	keep := url.Values{}
	for other, spec := range reportSortSpecs {
		if other == ns {
			continue
		}
		_, kv := tablesort.ParseValuesNS(q, spec, other)
		for k, vs := range kv {
			keep[k] = vs
		}
	}
	return keep
}

// reportSort resolves table ns's order from q and builds its header row at base,
// carrying the other tables' sorts.
func reportSort(q url.Values, ns, base string, cols []sortCol) (tablesort.Order, []templates.SortHeaderView) {
	spec := reportSortSpecs[ns]
	o, _ := tablesort.ParseValuesNS(q, spec, ns)
	keep := otherSorts(q, ns)
	return o, sortHeaders(base, cols, spec, o, ns, keep)
}

// refreshHref is the Refresh target of table ns's report: base plus that
// table's own validated sort params (an invalid or absent one is dropped), so
// a refresh re-runs the report in the order the address bar shows.
func refreshHref(q url.Values, ns, base string) string {
	_, own := tablesort.ParseValuesNS(q, reportSortSpecs[ns], ns)
	if len(own) == 0 {
		return base
	}
	return base + "?" + own.Encode()
}
