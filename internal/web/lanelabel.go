package web

import "github.com/sydlexius/canticle/internal/reports"

// laneLabel returns the user-facing name for a PERSISTED lane string. The
// mapping lives in reports.LaneLabel so the Reports Source sort (#1260) orders
// by the very text this renders; see there for the rationale.
func laneLabel(lane string) string { return reports.LaneLabel(lane) }
