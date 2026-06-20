package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/reports"
	"github.com/sydlexius/mxlrcgo-svc/web/templates"
)

// dashboardRecentLimit caps the Recent outcomes section on the dashboard.
// Kept lower than the canned report (50) to keep the page scannable.
const dashboardRecentLimit = 20

// handleDashboard renders the read-only observability dashboard. It is gated
// by the same auth guard as the other UI routes and is never cached (it exposes
// queue state and config detail). When the reports repo is not wired (no DB
// seam) it returns 503 rather than rendering an empty page that reads as "no
// data" -- same fail-loudly pattern as handleReportFragment.
func (u *UI) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if u.reports == nil {
		slog.Error("dashboard: reports repo not wired; cannot serve dashboard")
		http.Error(w, "dashboard data source unavailable", http.StatusServiceUnavailable)
		return
	}
	view, err := u.buildDashboardView(r)
	if err != nil {
		slog.Error("dashboard: build view failed", "error", err)
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	render(w, r, templates.DashboardPage(u.version, u.buildRail(""), view))
}

// buildDashboardView queries the reports repo and assembles the dashboard view
// model. All formatting happens here; the template receives pre-rendered strings.
func (u *UI) buildDashboardView(r *http.Request) (templates.DashboardView, error) {
	ctx := r.Context()
	view := templates.DashboardView{
		AsOf: time.Now().Format(reportTimeFormat),
	}

	// Resolve the server display timezone for completed-at timestamps (P4).
	// If TZ env is set and valid, format server-side in that zone so the JS
	// client-side reformat does not fire (data-tz-applied gate).
	var serverLoc *time.Location
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			serverLoc = loc
		}
	}

	qs, err := u.reports.QueueSummary(ctx)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: queue summary: %w", err)
	}
	view.QueueTiles = buildQueueTiles(qs)

	pe, err := u.reports.ProviderEffectiveness(ctx)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: provider effectiveness: %w", err)
	}
	view.ProviderTiles = buildProviderTiles(pe)

	instrumental, err := u.reports.CountInstrumental(ctx)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: count instrumental: %w", err)
	}
	view.InstrumentalCount = strconv.FormatInt(instrumental, 10)

	recent, err := u.reports.RecentOutcomes(ctx, dashboardRecentLimit)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: recent outcomes: %w", err)
	}
	view.RecentRows = buildRecentRows(recent, serverLoc)

	return view, nil
}

// buildQueueTiles shapes a QueueSummary into the dashboard's queue stat tiles.
func buildQueueTiles(qs reports.QueueSummary) []templates.StatTile {
	return []templates.StatTile{
		{Label: "Pending", Value: strconv.FormatInt(qs.Pending, 10)},
		{Label: "Processing", Value: strconv.FormatInt(qs.Processing, 10)},
		{Label: "Done", Value: strconv.FormatInt(qs.Done, 10)},
		{Label: "Failed", Value: strconv.FormatInt(qs.Failed, 10)},
		{Label: "Deferred", Value: strconv.FormatInt(qs.Deferred, 10)},
	}
}

// buildProviderTiles shapes per-provider effectiveness rows into stat tiles.
func buildProviderTiles(pe []reports.ProviderEffectiveness) []templates.StatTile {
	tiles := make([]templates.StatTile, 0, len(pe))
	for _, p := range pe {
		attempts := p.Hits + p.Misses
		tiles = append(tiles, templates.StatTile{
			Label: p.Lane,
			Value: fmt.Sprintf("%d/%d", p.Hits, attempts),
			Sub:   fmt.Sprintf("%.0f%%", p.HitRate*100),
		})
	}
	return tiles
}

// buildRecentRows shapes recent outcomes into table rows, formatting each
// completed-at timestamp in serverLoc when set (see formatDashboardTime).
func buildRecentRows(recent []reports.RecentOutcome, serverLoc *time.Location) []templates.RecentOutcomeRow {
	rows := make([]templates.RecentOutcomeRow, 0, len(recent))
	for _, o := range recent {
		display, iso, tzApplied := formatDashboardTime(o.CompletedAt, serverLoc)
		rows = append(rows, templates.RecentOutcomeRow{
			Artist:               o.Artist,
			Title:                o.Title,
			Album:                o.Album,
			Result:               string(o.Result),
			Lane:                 o.ProviderLane,
			CompletedAt:          display,
			CompletedAtISO:       iso,
			CompletedAtTZApplied: tzApplied,
		})
	}
	return rows
}

// formatDashboardTime formats a completed-at timestamp for the dashboard table.
// It returns (display, iso, tzApplied) where:
//   - display is the labeled human string shown server-side
//   - iso is the RFC3339 UTC value for the <time datetime=> attribute (empty for zero)
//   - tzApplied is true when loc was used, signaling JS should not reformat
func formatDashboardTime(t time.Time, loc *time.Location) (display, iso string, tzApplied bool) {
	if t.IsZero() {
		return "-", "", false
	}
	iso = t.UTC().Format(time.RFC3339)
	if loc != nil {
		return t.In(loc).Format("2006-01-02 15:04 MST"), iso, true
	}
	return t.UTC().Format("2006-01-02 15:04 UTC"), iso, false
}
