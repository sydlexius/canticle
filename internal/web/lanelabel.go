package web

import "github.com/sydlexius/canticle/internal/detectorbackfill"

// laneDisplayNames maps a PERSISTED lane string to the label shown in the UI.
//
// This is a presentation-only mapping (#539). The stored value is deliberately
// unchanged: it is the primary key of provider_outcomes and the value written to
// work_queue.provider_lane, so renaming it would split one lane's history across
// two keys and silently zero any query keyed on the old string. The key here is
// taken from detectorbackfill.LaneName rather than re-typing the literal, which
// is exported for exactly this reason (the equivalent constants in
// internal/worker and internal/orchestrator are unexported and unreachable).
//
// Lanes absent from this map render as-is, so adding a provider lane needs no
// change here.
var laneDisplayNames = map[string]string{
	detectorbackfill.LaneName: "Instrumental Detector",
}

// laneLabel returns the user-facing name for a persisted lane string, falling
// back to the raw value so an unmapped lane is still shown rather than blanked.
func laneLabel(lane string) string {
	if display, ok := laneDisplayNames[lane]; ok {
		return display
	}
	return lane
}
