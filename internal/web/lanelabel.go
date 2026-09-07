package web

import (
	"github.com/sydlexius/canticle/internal/detectorbackfill"
	"github.com/sydlexius/canticle/internal/providers"
)

// laneLabel returns the user-facing name for a PERSISTED lane string, falling
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
func laneLabel(lane string) string {
	switch lane {
	case detectorbackfill.LaneName:
		return "Instrumental Detector"
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
	default:
		return lane
	}
}
