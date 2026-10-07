package reports

import (
	"strings"

	"github.com/sydlexius/canticle/internal/detectorbackfill"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
)

// LaneLabel returns the user-facing name for a PERSISTED lane string, falling
// back to the raw value so an unmapped lane is still shown rather than blanked.
//
// This is a presentation-only mapping (#539). The stored value is deliberately
// unchanged: it is the primary key of provider_outcomes and the value written to
// work_queue.provider_lane, so renaming it would split one lane's history across
// two keys and silently zero any query keyed on the old string. The case below
// is taken from detectorbackfill.LaneName rather than re-typing the literal,
// which is exported for exactly this reason (the equivalent constants in
// internal/worker and internal/orchestrator are unexported and unreachable).
//
// A switch rather than a package-level map: the mapping is fixed at compile time
// and nothing should be able to mutate it at runtime, which a package-global map
// permits and Go cannot prevent. Lanes with no case render as-is, so adding a
// provider lane is a new case here and needs no change at any call site.
func LaneLabel(lane string) string {
	switch lane {
	case detectorbackfill.LaneName:
		return "Instrumental Detector"
	// The two single-source lanes read as the provider's own name (#1276), the
	// spelling the layout's credit line uses. Constants for the same reason.
	case providers.Musixmatch:
		return "Musixmatch"
	case providers.PetitLyrics:
		return "PetitLyrics"
	// Taken from providers.InnerTube for the same reason the detector's case is
	// taken from detectorbackfill: that constant IS the persisted value, so the
	// label can never drift from the lane it labels.
	//
	// The label names the LANE, not the licensor. This lane multiplexes, so the
	// upstream varies per track and no single provider name would be true of
	// every row aggregated under it -- the licensor is recorded per sidecar in
	// [upstream:] instead (docs/provider-attribution.md, #859). Naming a
	// licensor here would assert of a whole column something that is only
	// sometimes true.
	case providers.InnerTube:
		return "YouTube Music"
	// The lane a hand-marked instrumental carries (#1218, #1405).
	case queue.ManualLane:
		return "Manual"
	default:
		return lane
	}
}

// sourceLabelExpr is a SQL CASE mapping each persisted lane key to its
// LaneLabel text (the raw key for any other lane), so the Source sort orders
// what the cell shows. It is generated from LaneLabel over the same lane list
// the label is defined for (the providers plus the detector lane), once at
// package init, so the two cannot drift. Labels are code constants, never
// request input; single quotes are still doubled.
var sourceLabelExpr = buildSourceLabelExpr()

// buildSourceLabelExpr renders the SQL CASE that maps provider_lane to its displayed label, so a source sort orders by what the table shows.
func buildSourceLabelExpr() string {
	var b strings.Builder
	b.WriteString("CASE provider_lane")
	for _, k := range append(Lanes(), detectorbackfill.LaneName, queue.ManualLane) {
		b.WriteString(" WHEN '" + sqlQuote(k) + "' THEN '" + sqlQuote(LaneLabel(k)) + "'")
	}
	b.WriteString(" ELSE provider_lane END")
	return b.String()
}

// sqlQuote doubles single quotes so a label constant can sit inside a SQL string literal.
func sqlQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }
