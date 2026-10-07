package web

import (
	"context"
	"errors"
	"log/slog"
	"math"
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

// hasAttempts reports whether lane has recorded attempts, which is what gives
// it a dashboard tile; every tile link must resolve, so such a lane is served
// (as the empty state when it has no done rows) even when it is retired.
func (u *UI) hasAttempts(ctx context.Context, lane string) (bool, error) {
	pe, err := u.reports.ProviderEffectiveness(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(pe, func(p reports.ProviderEffectiveness) bool { return p.Lane == lane }), nil
}

func (u *UI) handleSource(w http.ResponseWriter, r *http.Request) {
	lane := r.PathValue("lane")
	u.serveSource(w, r, func(all []reports.SourceBreakdown) (reports.SourceBreakdown, bool, error) {
		for _, sb := range all {
			if !sb.Unattributed && sb.Lane == lane {
				return sb, true, nil
			}
		}
		if knownSourceLane(lane) {
			return reports.SourceBreakdown{Lane: lane}, true, nil
		}
		ok, err := u.hasAttempts(r.Context(), lane)
		return reports.SourceBreakdown{Lane: lane}, ok, err
	})
}

func (u *UI) handleSourceUnattributed(w http.ResponseWriter, r *http.Request) {
	u.serveSource(w, r, func(all []reports.SourceBreakdown) (reports.SourceBreakdown, bool, error) {
		for _, sb := range all {
			if sb.Unattributed {
				return sb, true, nil
			}
		}
		return reports.SourceBreakdown{Unattributed: true}, true, nil
	})
}

func (u *UI) serveSource(w http.ResponseWriter, r *http.Request, pick func([]reports.SourceBreakdown) (reports.SourceBreakdown, bool, error)) {
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
	sb, ok, err := pick(all)
	if err != nil {
		slog.Error("source page: provider effectiveness failed", "error", err)
		http.Error(w, "source breakdown failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	view := buildSourceView(sb, u.reports.TopRung())
	days, err := parseTrendRange(r.URL.Query()["range"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	view.Trend = templates.TrendView{Ranges: trendRanges(r.URL.EscapedPath(), days)}
	if sb.Unattributed {
		// The recorder ignores an empty lane, so this group has no daily counters.
		view.Trend.Note = "Daily history is not recorded for unattributed tracks: they have no source to count."
	} else if tr, err := u.reports.SourceTrend(r.Context(), sb.Lane, u.now(), days); err != nil {
		slog.Error("source page: trend failed", "error", err)
		http.Error(w, "source trend failed", http.StatusInternalServerError)
		return
	} else {
		fillTrend(&view.Trend, tr)
	}
	render(w, r, templates.SourcePage(u.version, view, u.buildRail(""), u.musixmatchInactive, u.musixmatchServing))
}

// trendRangeDays are the selectable windows; trendDefaultDays applies when the
// range parameter is absent, unknown or not in canonical spelling (a default of
// 30 applies). A repeated parameter is ambiguous and rejected with a 400, as
// parseQueueViewState does.
var trendRangeDays = []int{7, 30, 90}

const trendDefaultDays = 30

func parseTrendRange(vals []string) (int, error) {
	if len(vals) > 1 {
		return 0, errors.New("repeated parameter range")
	}
	// Only the canonical spelling selects a range: "+7" and "007" are NOT
	// accepted and fall back to the default.
	if len(vals) == 1 {
		for _, d := range trendRangeDays {
			if vals[0] == strconv.Itoa(d) {
				return d, nil
			}
		}
	}
	return trendDefaultDays, nil
}

func trendRanges(path string, cur int) []templates.TrendRange {
	var out []templates.TrendRange
	for _, d := range trendRangeDays {
		out = append(out, templates.TrendRange{Label: strconv.Itoa(d) + " days", Href: path + "?range=" + strconv.Itoa(d), Current: d == cur})
	}
	return out
}

// trendTypeLabels are the delivered-type series, the first four resultBuckets
// (word, line, unsynced, instrumental: the worker's landing events). Deriving
// them keeps the labels equal to the chart color keys, which
// TestResultBucketsHaveChartColors pins for every resultBuckets label.
func trendTypeLabels() []string {
	out := make([]string, 0, 4)
	for _, b := range resultBuckets[:4] {
		out = append(out, b.Label)
	}
	return out
}

// fillTrend turns the data-layer trend into chart series and table rows. A
// no-attempt day stays nil (a gap); the chart is skipped when it has no point.
func fillTrend(v *templates.TrendView, tr reports.SourceTrend) {
	if !tr.HasHistory {
		v.Note = "No daily counts in the last 90 days."
		return
	}
	hit := templates.TrendSeries{Label: "Hit rate (%)"}
	typ := map[string]*templates.TrendSeries{}
	order := trendTypeLabels()
	for _, l := range order {
		typ[l] = &templates.TrendSeries{Label: l}
	}
	for _, d := range tr.Days {
		v.Hit.Labels = append(v.Hit.Labels, d.Day)
		var p *float64
		cell := "-"
		if d.HitRate != nil {
			r := math.Round(*d.HitRate*10) / 10
			p, cell = &r, strconv.FormatFloat(r, 'f', 1, 64)+"%"
		}
		hit.Data = append(hit.Data, p)
		v.Hit.Details = append(v.Hit.Details, "Hits "+strconv.FormatInt(d.Hits, 10)+"  /  Misses "+strconv.FormatInt(d.Misses, 10))
		cells := []string{cell, strconv.FormatInt(d.Hits, 10), strconv.FormatInt(d.Misses, 10)}
		for i, n := range []int64{d.Word, d.Line, d.Unsynced, d.Instrumental} {
			f := float64(n)
			typ[order[i]].Data = append(typ[order[i]].Data, &f)
			cells = append(cells, strconv.FormatInt(n, 10))
		}
		v.TableRows = append(v.TableRows, templates.SourceRow{Label: d.Day, Cells: cells})
	}
	v.Hit.Series = []templates.TrendSeries{hit}
	v.Types.Labels = v.Hit.Labels
	for _, l := range order {
		v.Types.Series = append(v.Types.Series, *typ[l])
	}
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
	for _, b := range resultBuckets {
		if b.Label == label && b.Href != nil {
			return b.Href(top, sb.Lane)
		}
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
	t := templates.SourceTable{Heading: "By result type", ChartID: "mx-source-type-chart",
		Blurb: "Total " + strconv.FormatInt(sb.Counts.Total(), 10) + " completed tracks."}
	// Every type is a tile and a legend entry, zero included, as on the dashboard.
	for i, l := range labels {
		n := strconv.FormatInt(vals[i], 10)
		t.Tiles = append(t.Tiles, templates.StatTile{Label: l, Value: n, Href: sourceTypeLink(sb, l, top), Tooltip: resultBuckets[i].tooltip(top)})
		t.Chart.Labels = append(t.Chart.Labels, l)
		t.Chart.Values = append(t.Chart.Values, float64(vals[i]))
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
		up.Tiles = append(up.Tiles, templates.StatTile{Label: name, Value: cells[0]})
		up.Chart.Labels = append(up.Chart.Labels, name)
		up.Chart.Values = append(up.Chart.Values, float64(ub.Counts.Total()))
	}
	v.Upstream = &up
	return v
}
