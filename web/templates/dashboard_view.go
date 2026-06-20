package templates

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
	// RecentRows holds the most recently completed tracks (newest first, capped at 20).
	// Uses the shared RecentOutcomeRow type from reports_view.go.
	RecentRows []RecentOutcomeRow
	// AsOf is the formatted timestamp of this render, for the "as of" annotation.
	AsOf string
}

// StatTile is a single key-metric tile rendered in a dashboard tile row.
type StatTile struct {
	Label string // short human label, e.g. "Pending" or a provider lane name
	Value string // formatted numeric value
	Sub   string // optional annotation, e.g. "75.0% hit rate"; empty = not shown
}
