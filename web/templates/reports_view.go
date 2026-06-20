package templates

// Presentation models for the Reports workspace (#211). The handler maps the
// read-only reports.Repo return types onto these string-bearing view structs so
// the templ files stay free of formatting and database concerns. Every value is
// pre-formatted by the handler; templates only branch on emptiness.

// RailItem is one report row in the sidebar's REPORTS group.
type RailItem struct {
	// Key is the report's stable URL slug (e.g. "queue-summary"). Kept raw for
	// the active-state comparison; Path is the encoded value used for links.
	Key string
	// Path is the pre-computed, path-segment-encoded link target
	// (e.g. "/reports/queue-summary"). The handler builds it with
	// url.PathEscape so a key with reserved characters cannot break the URL.
	Path string
	// Title is the human label shown in the sidebar row.
	Title string
	// Active marks the currently-selected report (drives the accent highlight).
	Active bool
}

// ReportView is the right-pane view model for one rendered report. Exactly one
// of the row slices is populated, selected by Key; the rest are nil.
type ReportView struct {
	Key      string
	Title    string
	Subtitle string
	// LastRun is the formatted run timestamp for this execution; empty means
	// never run (the default-pane case never carries a populated ReportView).
	LastRun string

	QueueRows        []QueueSummaryRow
	RecentRows       []RecentOutcomeRow
	ProviderRows     []ProviderRow
	InstrumentalRows []InstrumentalRow
	FailureRows      []FailureRow
}

// QueueSummaryRow is one status/count pair. IsTotal marks the summary total row
// so the template can emphasize it.
type QueueSummaryRow struct {
	Status  string
	Count   string
	IsTotal bool
}

// RecentOutcomeRow is one recently-completed track with its derived result.
type RecentOutcomeRow struct {
	Artist      string
	Title       string
	Album       string
	Result      string
	Lane        string
	CompletedAt string
	// CompletedAtISO is the RFC3339 UTC value for the HTML <time datetime=> attribute.
	// Empty when CompletedAt is the zero sentinel "-".
	CompletedAtISO string
	// CompletedAtTZApplied is true when the server formatted CompletedAt using the
	// TZ env var, signaling that JS should not reformat it.
	CompletedAtTZApplied bool
}

// ProviderRow is one provider lane's hit/miss tally and true per-track hit-rate.
type ProviderRow struct {
	Lane    string
	Hits    string
	Misses  string
	HitRate string
}

// InstrumentalRow is one audio-detected instrumental track joined to its file.
type InstrumentalRow struct {
	ID              string
	Artist          string
	Title           string
	File            string
	DetectRequested string
}

// FailureRow is one failed/deferred group: status, reason, and count.
type FailureRow struct {
	Status string
	Reason string
	Count  string
}
