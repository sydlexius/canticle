package templates

import "encoding/json"

// Presentation model for the /dashboard observability page (#186). Like the
// reports view models, every field is pre-formatted by the handler; the template
// only branches on emptiness and renders strings.

// DashboardView is the view model for the read-only observability dashboard.
type DashboardView struct {
	// QueueTiles holds one tile per work-queue status
	// (pending, processing, done, failed, deferred) plus Instrumental.
	QueueTiles []StatTile
	// ProviderTiles holds one tile per provider lane showing hit count + hit rate.
	ProviderTiles []StatTile
	// InstrumentalCount is the formatted count of audio-detected instrumental tracks.
	InstrumentalCount string
	// QueueChart holds the work-queue status distribution for the doughnut chart
	// (#318). It complements the queue tiles; it is omitted when every count is
	// zero (HasData false), so an empty queue does not render a blank chart.
	QueueChart ChartData
	// RecentRows holds the most recently completed tracks (newest first, capped at 20).
	// Uses the shared RecentOutcomeRow type from reports_view.go.
	RecentRows []RecentOutcomeRow
	// AsOf is the formatted timestamp of this render, for the "as of" annotation.
	AsOf string
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
}
