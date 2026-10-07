package web

import (
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/sydlexius/canticle/internal/detectorbackfill"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/web/templates"
)

// unattributedPath is the page for done rows with no provider_lane. It is two
// path segments, the first a literal "-", so it can never match the one-segment
// /sources/{lane} route and no lane name can collide with it.
const unattributedPath = "/sources/-/unattributed"

// sourceHref is the per-source page for a persisted lane name.
func sourceHref(lane string) string { return "/sources/" + url.PathEscape(lane) }

// registerSourceRoutes registers the source drill-down routes (#1300) through reg (session-guarded).
func (u *UI) registerSourceRoutes(reg routeReg) {
	reg("GET /sources/{lane}", u.handleSource)
	reg("GET "+unattributedPath, u.handleSourceUnattributed)
}

// knownSourceLane: a lane that may have a page with no rows. A retired lane has one only with rows.
func knownSourceLane(lane string) bool {
	return reports.ValidLane(lane) || lane == detectorbackfill.LaneName
}

func (u *UI) handleSource(w http.ResponseWriter, r *http.Request) {
	lane := r.PathValue("lane")
	u.serveSource(w, r, func(all []reports.SourceBreakdown) (reports.SourceBreakdown, bool) {
		for _, sb := range all {
			if !sb.Unattributed && sb.Lane == lane {
				return sb, true
			}
		}
		return reports.SourceBreakdown{Lane: lane}, knownSourceLane(lane)
	})
}

func (u *UI) handleSourceUnattributed(w http.ResponseWriter, r *http.Request) {
	u.serveSource(w, r, func(all []reports.SourceBreakdown) (reports.SourceBreakdown, bool) {
		for _, sb := range all {
			if sb.Unattributed {
				return sb, true
			}
		}
		return reports.SourceBreakdown{Unattributed: true}, true
	})
}

func (u *UI) serveSource(w http.ResponseWriter, r *http.Request, pick func([]reports.SourceBreakdown) (reports.SourceBreakdown, bool)) {
	w.Header().Set("Cache-Control", "no-store")
	if u.reports == nil {
		slog.Error("reports repo not wired; cannot serve source page")
		http.Error(w, "source data unavailable", http.StatusServiceUnavailable)
		return
	}
	all, err := u.reports.SourceBreakdown(r.Context())
	if err != nil {
		slog.Error("source page: breakdown failed", "error", err)
		http.Error(w, "source breakdown failed", http.StatusInternalServerError)
		return
	}
	sb, ok := pick(all)
	if !ok {
		http.NotFound(w, r)
		return
	}
	render(w, r, templates.SourcePage(u.version, buildSourceView(sb, u.reports.TopRung()), u.buildRail(""), u.musixmatchInactive, u.musixmatchServing))
}

// typeCellsFor lists counts in resultBuckets order (the Results tiles' labels).
func typeCellsFor(c reports.TypeCounts, top reports.TopRung) (labels []string, vals []int64) {
	rb := reports.ResultsBreakdown{WordSynced: c.WordSynced, LineSynced: c.LineSynced, Unsynced: c.Unsynced,
		Instrumental: c.Instrumental, SyncedTierUnknown: c.TierUnknown, Other: c.Other, TopRung: top}
	for _, b := range resultBuckets {
		labels = append(labels, b.Label)
		vals = append(vals, b.Value(rb))
	}
	return labels, vals
}

// sourceTypeLink is the queue listing holding exactly a source's done rows of
// one type, or "" when none does. Only a selectable provider lane links (the
// detector and the NULL group are not lane= values), and only the two types
// the existing buckets isolate: word-synced (Finished, word rung only) and
// line-synced (the chip, wherever BucketChips offers it). The rest share a
// bucket with other types, so they stay plain text.
func sourceTypeLink(sb reports.SourceBreakdown, label string, top reports.TopRung) string {
	if sb.Unattributed || !reports.ValidLane(sb.Lane) {
		return ""
	}
	switch label {
	case "Word-synced":
		if top == reports.TopRungLine {
			return ""
		}
		return resultsHref(reports.BucketFinished, queueViewState{Lane: sb.Lane})
	case "Line-synced":
		b := reports.BucketSettled
		if top == reports.TopRungLine {
			b = reports.BucketFinished
		}
		if !reports.HasChip(b, reports.ChipLineSynced, top) {
			return ""
		}
		return resultsHref(b, queueViewState{Lane: sb.Lane, Tier: reports.TierLine})
	}
	return ""
}

func buildSourceView(sb reports.SourceBreakdown, top reports.TopRung) templates.SourceView {
	v := templates.SourceView{Mark: laneMark(sb.Lane), Empty: sb.Counts.Total() == 0}
	switch {
	case sb.Unattributed:
		v.Name, v.Mark = "Unattributed", markNone
		v.Blurb = "Completed tracks with no recorded source: served from cache, or finished before sources were recorded."
	default:
		v.Name = laneLabel(sb.Lane)
		v.Blurb = "Completed tracks this source delivered, by result type."
	}
	if v.Empty {
		return v
	}
	labels, vals := typeCellsFor(sb.Counts, top)
	t := templates.SourceTable{Heading: "By result type", FirstCol: "Type", Cols: []string{"Tracks"}, ChartID: "mx-source-type-chart",
		Blurb: "Total " + strconv.FormatInt(sb.Counts.Total(), 10) + " completed tracks."}
	for i, l := range labels {
		t.Rows = append(t.Rows, templates.SourceRow{Label: l, Cells: []string{strconv.FormatInt(vals[i], 10)}, Href: sourceTypeLink(sb, l, top)})
		if vals[i] > 0 {
			t.Chart.Labels = append(t.Chart.Labels, l)
			t.Chart.Values = append(t.Chart.Values, float64(vals[i]))
		}
	}
	v.Types = t
	if len(sb.Upstreams) == 0 || sb.Unattributed || !slices.Contains(providers.UpstreamLanes(), sb.Lane) {
		return v
	}
	up := templates.SourceTable{Heading: "By upstream", FirstCol: "Upstream", ChartID: "mx-source-upstream-chart",
		Blurb: "Which licensor the source reported for each track. Rows are not linked: the queue has no upstream filter."}
	up.Cols = append([]string{"Tracks"}, labels...)
	for _, ub := range sb.Upstreams {
		name := ub.Upstream
		if ub.NotRecorded {
			name = "Not recorded"
		}
		_, uv := typeCellsFor(ub.Counts, top)
		cells := []string{strconv.FormatInt(ub.Counts.Total(), 10)}
		for _, n := range uv {
			cells = append(cells, strconv.FormatInt(n, 10))
		}
		up.Rows = append(up.Rows, templates.SourceRow{Label: name, Cells: cells})
		up.Chart.Labels = append(up.Chart.Labels, name)
		up.Chart.Values = append(up.Chart.Values, float64(ub.Counts.Total()))
	}
	v.Upstream = &up
	return v
}
