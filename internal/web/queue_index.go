package web

import (
	"log/slog"
	"net/http"

	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/web/templates"
)

// handleQueueIndex renders the /queue landing page (#1241): the dashboard's own
// queue tiles (buildQueueTiles over the one QueueSummary), as a list of links
// to the bucket drill-downs. No query or predicate of its own.
func (u *UI) handleQueueIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if u.reports == nil {
		slog.Error("reports repo not wired; cannot serve queue index")
		http.Error(w, "queue data source unavailable", http.StatusServiceUnavailable)
		return
	}
	qs, err := u.reports.QueueSummary(r.Context())
	if err != nil {
		slog.Error("queue index: queue summary failed", "error", err)
		http.Error(w, "queue summary failed", http.StatusInternalServerError)
		return
	}
	render(w, r, templates.QueueIndexPage(u.version, buildQueueTiles(qs), u.buildRail(""), u.musixmatchInactive, u.musixmatchServing,
		qs.TopRung == reports.TopRungLine))
}
