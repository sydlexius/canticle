package web

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/web/templates"
)

// dashboardRecentLimit caps the Recent outcomes section on the dashboard.
// Kept lower than the canned report (50) to keep the page scannable.
const dashboardRecentLimit = 20

// dashboardUpNextLimit caps the "Up next" panel (#572). The lookahead buffer is
// DB-bounded by queue.batch_size (default 10), so a cap comfortably above any
// realistic batch size means the panel shows the whole buffer and the "N
// buffered" header equals the rendered row count. An operator running a larger
// batch_size sees the first N; the buffer never lists more than batch_size rows.
const dashboardUpNextLimit = 50

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
	render(w, r, templates.DashboardPage(u.version, u.buildRail(""), view, u.musixmatchInactive))
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
	view.QueueChart = buildQueueChart(qs)

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

	upNext, err := u.reports.UpNext(ctx, dashboardUpNextLimit)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: up next: %w", err)
	}
	elig, err := u.reports.QueueEligibility(ctx)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: queue eligibility: %w", err)
	}
	view.UpNextRows = buildUpNextRows(upNext, time.Now())
	view.UpNextHeader = fmt.Sprintf("%d buffered of %s eligible",
		len(view.UpNextRows), groupThousands(elig.Eligible))
	view.UpNextEmpty = fmt.Sprintf("Nothing buffered. %s eligible, %s waiting on cooldown.",
		groupThousands(elig.Eligible), groupThousands(elig.Cooldown))

	return view, nil
}

// buildUpNextRows shapes buffered work items into ordered panel rows (#572),
// carrying artist/title/album as distinct columns, mapping the priority tier to
// a label, and rendering the compact "waited" age from created_at against now.
func buildUpNextRows(items []reports.UpNextItem, now time.Time) []templates.UpNextRow {
	rows := make([]templates.UpNextRow, 0, len(items))
	for i, it := range items {
		rows = append(rows, templates.UpNextRow{
			Position: strconv.Itoa(i + 1),
			Artist:   it.Artist,
			Title:    it.Title,
			Album:    it.Album,
			Tier:     tierLabel(it.Priority),
			Waited:   formatWaited(it.CreatedAt, now),
		})
	}
	return rows
}

// groupThousands formats n with comma thousands separators (e.g. 10102 ->
// "10,102"), keeping the large eligible/cooldown counts readable in the panel
// header and empty state. Negative values keep the sign.
func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := ""
	if n < 0 {
		neg, s = "-", s[1:]
	}
	if len(s) <= 3 {
		return neg + s
	}
	// Insert a comma every three digits from the right.
	var b []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, c)
	}
	return neg + string(b)
}

// tierLabel maps a raw work_queue.priority to the panel's tier label. Negative
// priorities are the deferred benign-miss tier (queue.PriorityMiss = -100);
// everything at or above the scan baseline (0) reads as "fresh". Kept in the
// handler so relabeling never touches the query or template.
func tierLabel(priority int) string {
	if priority < 0 {
		return "miss"
	}
	return "fresh"
}

// formatWaited renders how long an item has waited as one dominant unit (s, m,
// h, d), matching the compact mock ("2m", "6d"). A zero timestamp or a
// non-positive span renders "0s" so the column is never blank or negative.
func formatWaited(since, now time.Time) string {
	if since.IsZero() {
		return "0s"
	}
	d := now.Sub(since)
	switch {
	case d < time.Minute:
		if d < 0 {
			d = 0
		}
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
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

// buildQueueChart shapes a QueueSummary into the work-queue doughnut chart
// series (#318). The label order is fixed and matches the queue tiles so the
// chart-init color map (keyed by label) stays in sync. Total is intentionally
// excluded -- it is the sum of the segments, not a segment.
func buildQueueChart(qs reports.QueueSummary) templates.ChartData {
	return templates.ChartData{
		Labels: []string{"Pending", "Processing", "Done", "Failed", "Deferred"},
		Values: []float64{
			float64(qs.Pending),
			float64(qs.Processing),
			float64(qs.Done),
			float64(qs.Failed),
			float64(qs.Deferred),
		},
	}
}

// hitRatePct rounds a 0-1 hit rate to an integer percent (0-100). It is the
// single source for both the displayed "%" sub-label and the mini hit-rate
// bar's data-hit-rate value, so the bar width always matches the text (#318).
func hitRatePct(rate float64) int {
	return int(math.Round(rate * 100))
}

// hitRateBarFields returns the Sub label and the inline hit-rate bar fields
// (#318) for a tile carrying a 0-1 hit rate. ShowBar is always true here; the
// caller wires it onto a StatTile. The percent feeds both the text and the bar.
func hitRateBarFields(rate float64) (sub, barPct, barLabel string) {
	pct := hitRatePct(rate)
	sub = fmt.Sprintf("%d%%", pct)
	barPct = strconv.Itoa(pct)
	barLabel = fmt.Sprintf("Hit rate %d%%", pct)
	return sub, barPct, barLabel
}

// buildProviderTiles shapes per-provider effectiveness rows into stat tiles,
// each carrying its hit rate as an inline mini bar (#318).
func buildProviderTiles(pe []reports.ProviderEffectiveness) []templates.StatTile {
	tiles := make([]templates.StatTile, 0, len(pe))
	for _, p := range pe {
		attempts := p.Hits + p.Misses
		sub, barPct, barLabel := hitRateBarFields(p.HitRate)
		tiles = append(tiles, templates.StatTile{
			Label:    laneLabel(p.Lane),
			Value:    fmt.Sprintf("%d/%d", p.Hits, attempts),
			Sub:      sub,
			ShowBar:  true,
			BarPct:   barPct,
			BarLabel: barLabel,
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
			Lane:                 laneLabel(o.ProviderLane),
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
