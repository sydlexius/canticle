package templates

import (
	"encoding/json"
	"strconv"
)

// Presentation model for the /dashboard observability page (#186). Like the
// reports view models, every field is pre-formatted by the handler; the template
// only branches on emptiness and renders strings.

// DashboardView is the view model for the read-only observability dashboard.
type DashboardView struct {
	// Status is the one-line result of a mark action (#1433), shown at the top;
	// empty renders nothing.
	Status string
	// QueueTiles holds one tile per work-queue status
	// (pending, processing, done, failed, deferred); statuses only (#599).
	QueueTiles []StatTile
	// ProviderTiles holds one tile per provider lane showing hit count + hit rate,
	// then the Unattributed tile (#1422) when any result has no recorded source.
	ProviderTiles []StatTile
	// ResultsTiles holds the Results row (#599): completed tracks split by
	// result type. Always every bucket, so the tiles sum to Done.
	ResultsTiles []StatTile
	// QueueChart holds the work-queue status distribution for the doughnut chart
	// (#318). It complements the queue tiles; it is omitted when every count is
	// zero (HasData false), so an empty queue does not render a blank chart.
	QueueChart ChartData
	// RecentRows holds the most recently completed tracks (newest first, capped at 20).
	// Uses the shared RecentOutcomeRow type from reports_view.go.
	RecentRows []RecentOutcomeRow
	// AttentionRows holds failed then deferred rows (#654 AC2), capped at 10:
	// work with no lyric outcome, kept OUT of RecentRows' outcome column.
	AttentionRows []AttentionRow
	// AttentionLimit is the cap the handler applied to AttentionRows, carried
	// so the section's tooltip states the real number rather than a copy of it.
	AttentionLimit int
	// UpNextRows holds the buffered upcoming work in worker-claim order (#572).
	// Empty when the lookahead buffer is empty or batching is disabled, which
	// drives the panel's counts-only empty state.
	UpNextRows []UpNextRow
	// UpNextHeader is the pre-formatted "N buffered of M eligible" line shown
	// above the table when rows are present. UpNextEmpty is the counts-only line
	// (eligible + cooldown, no ordering claim) shown when UpNextRows is empty.
	// The template picks one by branching on len(UpNextRows); all formatting
	// (including thousands grouping) is done in the handler.
	UpNextHeader string
	UpNextEmpty  string
	// InFlightRows holds the claimed (processing) rows shown above the buffered
	// rows in the Up Next panel (#599). Several may coexist (a live claim and a
	// crash orphan).
	InFlightRows []InFlightRow
	// AsOf is the formatted timestamp of this render, for the "as of" annotation.
	AsOf string
	// LRCNormalizeSummary is the pre-formatted "last LRC normalization" line
	// (#929): how many stacked .lrc sidecars the most recent applied
	// `scan reconcile-lrc --yes` pass rewrote, and when. Carries counts and a
	// timestamp only -- never a path, artist, title, or album -- matching
	// every other aggregate on this page. Always non-empty; the handler
	// renders a distinct, non-alarming sentence for the "never run yet"
	// state rather than leaving this blank.
	LRCNormalizeSummary string
}

// UpNextRow is one buffered work item in the dashboard "Up next" panel (#572).
// Every field is pre-formatted by the handler; the template only renders strings.
type UpNextRow struct {
	// Actions is the row's mark icon (#1434); the zero value (ID 0) renders none.
	Actions RowActions
	// Position is the 1-based rank in claim order (the buffer sequence), as a
	// pre-formatted string.
	Position string
	// Artist, Title, and Album are the track identity, each in its own column.
	Artist string
	Title  string
	Album  string
	// Tier is the priority-tier label ("miss" or "fresh").
	Tier string
	// Waited is the compact single-unit age of the item (e.g. "2m", "6d").
	Waited string
}

// InFlightRow is one claimed work item in the Up Next panel (#599). Elapsed is
// pre-formatted ("2m", or "unknown" when the claim time is not recorded); Stuck
// drives the "stuck?" badge.
type InFlightRow struct {
	Artist  string
	Title   string
	Album   string
	Elapsed string
	Stuck   bool
}

// ChartData is the label/value series for one dashboard chart (#318). The
// handler builds it; the template only serializes it into canvas data
// attributes for the vendored, CSP-safe Chart.js init script to read. Values
// are plain numbers (counts for the queue doughnut, hit-rate percentages for
// the provider bar chart); colors are resolved client-side from design tokens,
// so this model stays presentation-agnostic.
type ChartData struct {
	Labels []string  // segment/bar labels, parallel to Values
	Values []float64 // numeric values, parallel to Labels
}

// HasData reports whether the chart has at least one non-zero value. A series of
// all zeros (e.g. an empty queue, or providers with no recorded attempts) is
// treated as "no data" so the template omits the chart rather than rendering a
// blank canvas.
func (c ChartData) HasData() bool {
	for _, v := range c.Values {
		if v != 0 {
			return true
		}
	}
	return false
}

// LabelsJSON returns the chart labels as a JSON array string for a canvas data
// attribute. templ HTML-escapes the attribute value on render; the browser
// unescapes it back to valid JSON for JSON.parse. Errors are not expected for
// a plain []string and collapse to an empty array so the init script fails
// loudly (parses [], renders nothing) rather than emitting malformed markup.
func (c ChartData) LabelsJSON() string { return marshalJSON(c.Labels) }

// ValuesJSON returns the chart values as a JSON array string (see LabelsJSON).
func (c ChartData) ValuesJSON() string { return marshalJSON(c.Values) }

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// StatTile is a single key-metric tile rendered in a dashboard tile row.
type StatTile struct {
	Label string // short human label, e.g. "Pending" or a provider lane name
	Value string // formatted numeric value
	Sub   string // optional annotation, e.g. "75.0% hit rate"; empty = not shown
	// Href, when set, makes the tile a link to that drill-down page (#598); empty
	// renders a plain tile.
	Href string
	// Tooltip is the hover text for a work-queue tile, carried from the one
	// bucket definition (internal/web queueBuckets, #599). Other tile rows
	// leave it empty.
	Tooltip string
	// LabelMark is the lane mark token shown beside Label (#601), empty when the
	// tile has no mark. The work-queue tiles leave it empty -- they are not lanes.
	LabelMark string
	// ShowBar gates the inline mini hit-rate bar (#318). Set for provider tiles
	// that carry a hit-rate percentage; the work-queue tiles leave it false so no
	// bar renders.
	ShowBar bool
	// BarPct is the integer hit-rate percent (0-100) as a string, emitted in the
	// fill's data-hit-rate attribute. chart-init.js applies it as the fill width
	// via the CSSOM (the serve-mode CSP forbids inline style="" attributes).
	BarPct string
	// BarLabel is the title/aria text for the mini-bar, e.g. "Hit rate 75%".
	BarLabel string
	// Status is the lane circuit state class suffix (#488): "healthy", "ready",
	// "probing", "throttled", "failing" or "inactive"; empty when no lane-health
	// source is wired (or the lane is Local), which renders no status line.
	// StatusText is the visible words, so the state never rests on color alone.
	Status     string
	StatusText string
}

// attentionTooltip is the Needs Attention section's hover text, naming the cap
// the handler actually applied.
func attentionTooltip(limit int) string {
	return "Up to " + strconv.Itoa(limit) + " rows: failed tracks first, then deferred ones, each newest attempt first."
}

// providerTileTitle is the hover text of a linked Lyrics Sources tile: the
// tile's own Tooltip when set (the Unattributed tile opens a track list, not a
// by-type source page), else the by-type source default.
func providerTileTitle(t StatTile) string {
	if t.Tooltip != "" {
		return t.Tooltip
	}
	return "Results by type for this source"
}
