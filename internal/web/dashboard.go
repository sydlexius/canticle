package web

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/sydlexius/canticle/internal/detectorbackfill"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/web/templates"
)

// dashboardRecentLimit caps the Recent outcomes section on the dashboard.
// Kept lower than the canned report (50) to keep the page scannable.
const dashboardRecentLimit = 20

// dashboardAttentionLimit caps the Needs attention section (#654 AC2), kept
// small so failed and deferred rows never crowd the page.
const dashboardAttentionLimit = 10

// dashboardUpNextLimit caps the "Up next" panel (#572). The lookahead buffer is
// DB-bounded by queue.batch_size (default 10), so a cap comfortably above any
// realistic batch size means the panel shows the whole buffer and the "N
// buffered" header equals the rendered row count. An operator running a larger
// batch_size sees the first N; the buffer never lists more than batch_size rows.
const dashboardUpNextLimit = 50

// dashboardStuckAfter is how long a row may sit in 'processing' before the Up
// Next panel flags it "stuck?" (#599, assumption A4). It is a display hint, not
// a verdict. The worst legitimate single item is bounded by the real client
// timeouts: the detector (3m, detector/http.go) and verification (2m,
// verification/verification.go) calls plus up to three provider lanes at 30s
// each (musixmatch/petitlyrics/innertube clients), about 6.5 minutes, so 10
// minutes sits clearly past any live claim. An orphan left by a crash stays
// 'processing' until startup recovery and so ages past it.
const dashboardStuckAfter = 10 * time.Minute

// AttachLaneHealth wires the per-lane circuit-state source onto the dashboard
// (#488). Pass a method value on the owner (worker.LaneHealth), never a captured
// orchestrator: it is called on every dashboard request and must see the
// current lanes after a rebuild.
func (u *UI) AttachLaneHealth(fn func() []orchestrator.LaneState) { u.laneHealth = fn }

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
	render(w, r, templates.DashboardPage(u.version, u.buildRail(""), view, u.musixmatchInactive, u.musixmatchServing))
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
	if u.laneHealth != nil {
		view.ProviderTiles = providerTilesWithHealth(pe, u.laneHealth(), u.musixmatchInactive, time.Now())
	} else {
		view.ProviderTiles = buildProviderTiles(pe)
	}

	sources, err := u.reports.SourceBreakdown(ctx)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: source breakdown: %w", err)
	}
	if t, ok := buildUnattributedTile(sources); ok {
		view.ProviderTiles = append(view.ProviderTiles, t)
	}

	results, err := u.reports.ResultsBreakdown(ctx)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: results breakdown: %w", err)
	}
	view.ResultsTiles = buildResultsTiles(results)

	recent, err := u.reports.RecentOutcomes(ctx, dashboardRecentLimit)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: recent outcomes: %w", err)
	}
	view.RecentRows = buildRecentRows(recent, serverLoc)
	stampRecentRelative(view.RecentRows, recent, time.Now())

	attention, err := u.reports.NeedsAttention(ctx, dashboardAttentionLimit)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: needs attention: %w", err)
	}
	view.AttentionRows = buildAttentionRows(attention, serverLoc)
	view.AttentionLimit = dashboardAttentionLimit

	inFlight, err := u.reports.InFlight(ctx)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: in flight: %w", err)
	}
	view.InFlightRows = buildInFlightRows(inFlight, time.Now())

	upNext, err := u.reports.UpNext(ctx, dashboardUpNextLimit)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: up next: %w", err)
	}
	elig, err := u.reports.QueueEligibility(ctx)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: queue eligibility: %w", err)
	}
	view.UpNextRows = buildUpNextRows(upNext, time.Now())
	// Header count is the TRUE buffered total from the DB, not len(rows): the
	// displayed list is capped at dashboardUpNextLimit, so a larger buffer
	// (queue.batch_size > cap) must still report its real size (#572 CR).
	view.UpNextHeader = fmt.Sprintf("%d buffered of %s eligible",
		elig.Buffered, groupThousands(elig.Eligible))
	// "retry backoff", never "cooldown": this count is governed by
	// api.miss_backoff_base_hours, while every config key spelled "cooldown" is
	// request PACING. The old wording pointed an operator at a knob that cannot
	// move this number; the lever is `queue recheck --deferred`.
	view.UpNextEmpty = fmt.Sprintf("Nothing buffered. %s eligible, %s waiting on retry backoff.",
		groupThousands(elig.Eligible), groupThousands(elig.RetryBackoff))

	norm, err := u.reports.LastLRCNormalization(ctx)
	if err != nil {
		return templates.DashboardView{}, fmt.Errorf("dashboard: last lrc normalization: %w", err)
	}
	view.LRCNormalizeSummary = formatLRCNormalizeSummary(norm, serverLoc)

	return view, nil
}

// formatLRCNormalizeSummary renders the #929 "last LRC normalization" line
// from the report's summary. It carries only a count and a timestamp -- never
// a path, artist, title, or album, matching every other dashboard aggregate.
//
// THREE DISTINCT SENTENCES, not two (follow-up to #929's original two-branch
// version):
//
//   - Ever false: no apply pass has ever run. Its own sentence rather than a
//     bare "0 files" -- a fresh install and a deployment that genuinely
//     rewrote zero sidecars are different states, and collapsing them would
//     read the pre-pass state as "checked, found nothing" when nothing has
//     actually been checked yet (this reports applies only, see
//     reports.Repo.LastLRCNormalization's doc comment for why the
//     marker-gated startup discovery pass never counts as a run here).
//   - Ever true, Normalized == 0: a pass DID run and genuinely found nothing
//     to rewrite. This is deliberately worded as a clean bill of health
//     ("no stacked sidecars found"), not as "0 files rewritten" -- the
//     marker is stamped on EVERY applied run, including a no-op one (see
//     internal/commands.markLRCNormalizeApply's doc comment: this is the
//     more honest record of "when did I last run this, and what happened"),
//     so this state is common and must not read as "the feature did
//     nothing" when it is actually reporting the library is already clean.
//   - Ever true, Normalized > 0: the original count sentence, unchanged.
func formatLRCNormalizeSummary(s reports.LRCNormalizationSummary, loc *time.Location) string {
	if !s.Ever {
		// No backticks around the command: this string is interpolated into the
		// template as escaped plain text, so a Markdown convention renders as
		// literal punctuation rather than as code formatting.
		return "No LRC normalization pass has run yet. Run \"canticle scan reconcile-lrc --yes\" to expand any stacked (multi-timestamp) .lrc sidecars."
	}
	display, _, _ := formatDashboardTime(s.CompletedAt, loc)
	if s.Normalized == 0 {
		return fmt.Sprintf("Last LRC normalization: %s -- no stacked sidecars found.", display)
	}
	noun := "file"
	if s.Normalized != 1 {
		noun = "files"
	}
	return fmt.Sprintf("Last LRC normalization: %d %s rewritten, %s.", s.Normalized, noun, display)
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

// buildInFlightRows shapes claimed (status='processing') rows into the panel's
// top rows (#599). Elapsed is time since the claim; an unknown claim time
// (zero) renders "unknown" and is never flagged stuck, since no age is known.
// Several rows are expected: a live claim can sit beside a crash orphan.
func buildInFlightRows(items []reports.InFlightItem, now time.Time) []templates.InFlightRow {
	rows := make([]templates.InFlightRow, 0, len(items))
	for _, it := range items {
		row := templates.InFlightRow{
			Artist:  it.Artist,
			Title:   it.Title,
			Album:   it.Album,
			Elapsed: "unknown",
		}
		if !it.ClaimedAt.IsZero() {
			row.Elapsed = formatWaited(it.ClaimedAt, now)
			row.Stuck = now.Sub(it.ClaimedAt) > dashboardStuckAfter
		}
		rows = append(rows, row)
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
// priorities are the deferred benign-miss tier (queue.PriorityMiss = -100),
// except exactly queue.PriorityUpgrade (-50), the upgrade-sweep tier, which has
// its own label; everything at or above the scan baseline (0) reads as "fresh".
// Kept in the handler so relabeling never touches the query or template.
func tierLabel(priority int) string {
	if priority == queue.PriorityUpgrade {
		return "upgrade"
	}
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
	if d < time.Minute {
		if d < 0 {
			d = 0
		}
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
	n, unit := spanBucket(d)
	suffix := map[time.Duration]string{time.Minute: "m", time.Hour: "h", 24 * time.Hour: "d"}[unit]
	return strconv.Itoa(n) + suffix
}

// buildQueueTiles shapes a QueueSummary into the dashboard's queue stat tiles,
// one per queueBuckets entry (#599), so label, tooltip, value and drill-down
// href (#598) all come from the one bucket definition.
func buildQueueTiles(qs reports.QueueSummary) []templates.StatTile {
	tiles := make([]templates.StatTile, 0, len(queueBuckets))
	for _, b := range queueBuckets {
		tiles = append(tiles, templates.StatTile{
			Label:   b.Label,
			Value:   strconv.FormatInt(b.Value(qs), 10),
			Href:    queueBucketHref(b.Key),
			Tooltip: b.tooltip(qs.TopRung),
		})
	}
	return tiles
}

// queueBucketHref is the /queue/{bucket} drill-down URL for a bucket, the
// inverse of the route registerQueueRoutes serves.
func queueBucketHref(b reports.Bucket) string { return "/queue/" + string(b) }

// buildResultsTiles shapes a ResultsBreakdown into the dashboard Results tiles
// (#599), one per resultBuckets entry. Every tile is always present, zero
// included, so the row is a checkable sum of the completed population; the
// word/line/tier-unknown tiles stay separate (#553, #627), never one "Synced".
func buildResultsTiles(b reports.ResultsBreakdown) []templates.StatTile {
	tiles := make([]templates.StatTile, 0, len(resultBuckets))
	for _, rb := range resultBuckets {
		tip := rb.tooltip(b.TopRung)
		href := ""
		if rb.Href != nil {
			href = rb.Href(b.TopRung, "")
		}
		tiles = append(tiles, templates.StatTile{
			Label:   rb.Label,
			Value:   strconv.FormatInt(rb.Value(b), 10),
			Href:    href,
			Tooltip: tip,
		})
	}
	return tiles
}

// buildQueueChart shapes a QueueSummary into the work-queue doughnut chart
// series (#318), one segment per queueBuckets entry in the same order as the
// tiles, so the chart-init color map (keyed by label) stays in sync. Total is
// intentionally excluded -- it is the sum of the segments, not a segment.
func buildQueueChart(qs reports.QueueSummary) templates.ChartData {
	c := templates.ChartData{
		Labels: make([]string, 0, len(queueBuckets)),
		Values: make([]float64, 0, len(queueBuckets)),
	}
	for _, b := range queueBuckets {
		c.Labels = append(c.Labels, b.Label)
		c.Values = append(c.Values, float64(b.Value(qs)))
	}
	return c
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
		tiles = append(tiles, buildProviderTile(p))
	}
	return tiles
}

// buildProviderTile shapes one lane's effectiveness row into its stat tile.
func buildProviderTile(p reports.ProviderEffectiveness) templates.StatTile {
	sub, barPct, barLabel := hitRateBarFields(p.HitRate)
	return templates.StatTile{
		Label:     laneLabel(p.Lane),
		LabelMark: laneMark(p.Lane),
		Value:     fmt.Sprintf("%d/%d", p.Hits, p.Hits+p.Misses),
		Sub:       sub,
		ShowBar:   true,
		BarPct:    barPct,
		BarLabel:  barLabel,
		Href:      sourceHref(p.Lane),
	}
}

// buildUnattributedTile is the Lyrics Sources tile for done results with no
// recorded source (#1422): a cache hit, a blocked track, or a row finished
// before sources were recorded. It reads the same SourceBreakdown group the
// /sources/-/unattributed page totals, so the two counts cannot differ. It is
// absent at zero, as a lane with no recorded attempts and no configured
// health gets no tile; it carries no hit rate, bar or status, which only a
// lane has.
func buildUnattributedTile(all []reports.SourceBreakdown) (templates.StatTile, bool) {
	for _, sb := range all {
		if !sb.Unattributed || sb.Counts.Total() == 0 {
			continue
		}
		return templates.StatTile{
			Label: "Unattributed",
			Value: strconv.FormatInt(sb.Counts.Total(), 10),
			Href:  unattributedPath,
		}, true
	}
	return templates.StatTile{}, false
}

// Lane status tokens carried on StatTile.Status; they are CSS class suffixes.
const (
	laneStatusHealthy   = "healthy"
	laneStatusReady     = "ready"
	laneStatusProbing   = "probing"
	laneStatusThrottled = "throttled"
	laneStatusFailing   = "failing"
	laneStatusInactive  = "inactive"
)

// providerTilesWithHealth builds the Lyrics Sources tiles when a lane-health
// source is attached (#488). The tile set is the UNION of the configured lanes
// (health, in priority order) and the lanes with recorded attempts (pe, then
// appended in pe order), so a lane whose breaker opened before it ever scored a
// hit or miss still gets a tile: that is the fresh-install bad-token case the
// status exists for. A health-only lane renders zero counts ("0/0", 0%), the
// same markup a recorded-but-empty lane gets. A Local lane (the detector) is
// not a lyrics source: it gets no status line, and no tile at all unless it has
// recorded attempts. In parallel mode, or with the detector off, it is absent
// from health but its history still renders a tile, identified by its
// persisted lane name (detectorbackfill.LaneName), still with no status line.
// Any other lane with attempts but absent from health is no longer configured
// and reads "Not active". When
// musixmatchInactive is set (no token: the worker never starts, the banner
// shows) the musixmatch tile reads inactive instead of its breaker state,
// which would otherwise say "Ready" for a lane that cannot run.
func providerTilesWithHealth(pe []reports.ProviderEffectiveness, health []orchestrator.LaneState, musixmatchInactive bool, now time.Time) []templates.StatTile {
	byLane := make(map[string]reports.ProviderEffectiveness, len(pe))
	for _, p := range pe {
		byLane[p.Lane] = p
	}
	seen := make(map[string]bool, len(health)+len(pe))
	tiles := make([]templates.StatTile, 0, len(health)+len(pe))
	for _, h := range health {
		p, recorded := byLane[h.Provider]
		if h.Local && !recorded {
			continue
		}
		seen[h.Provider] = true
		if !recorded {
			p = reports.ProviderEffectiveness{Lane: h.Provider}
		}
		t := buildProviderTile(p)
		switch {
		case h.Local:
		case musixmatchInactive && h.Provider == markMusixmatch:
			t.Status, t.StatusText = laneStatusInactive, "Inactive - add an API token"
		default:
			t.Status, t.StatusText = laneStatus(h, now)
		}
		tiles = append(tiles, t)
	}
	for _, p := range pe {
		if seen[p.Lane] {
			continue
		}
		seen[p.Lane] = true
		t := buildProviderTile(p)
		if p.Lane != detectorbackfill.LaneName {
			t.Status, t.StatusText = laneStatusInactive, "Not active"
		}
		tiles = append(tiles, t)
	}
	return tiles
}

// laneStatus maps one lane's breaker snapshot to its status class and words.
// Half-open is "probing", reported apart from open: the lane takes its next
// request even though it recently tripped. A refused lane (HTTP 403) says so
// in both states, since the refusal stands until a probe is answered. An open
// lane that has NEVER succeeded this session is "failing", not "throttled": that is the
// verify-your-token case (orchestrator resolve), and calling it throttling
// would send the operator waiting instead of fixing config. A closed lane that
// has not succeeded yet reads "Ready, no success this session" rather than "Healthy",
// because nothing has proven it healthy. The text states the status; color
// only reinforces it.
func laneStatus(h orchestrator.LaneState, now time.Time) (status, text string) {
	switch h.State {
	case orchestrator.LaneStateOpen:
		if h.Refused {
			// Not "Throttled": waiting does not lift a refusal (#1372).
			return laneStatusFailing, "Refused by the provider (HTTP 403), not throttling (" + retryIn(h.OpenUntil, now) + ")"
		}
		if !h.EverSucceeded {
			return laneStatusFailing, "Failing, no success this session - check token/config (" + retryIn(h.OpenUntil, now) + ")"
		}
		return laneStatusThrottled, "Throttled, " + retryIn(h.OpenUntil, now)
	case orchestrator.LaneStateHalfOpen:
		if h.Refused {
			// The refusal stands until a probe is answered (#1372).
			return laneStatusFailing, "Refused by the provider (HTTP 403), probing again"
		}
		return laneStatusProbing, "Probing"
	default:
		if !h.EverSucceeded {
			return laneStatusReady, "Ready, no success this session"
		}
		return laneStatusHealthy, "Healthy"
	}
}

// retryIn renders the relative countdown to until: "retry in 4m", "retry in
// 1h 5m", or "retry shortly" once the window has already passed (a snapshot can
// read open a moment after OpenUntil). Minutes round UP (ceiling): a "retry in"
// must never promise a retry sooner than the window allows, so 61s reads "2m"
// and 59m31s reads "1h".
func retryIn(until, now time.Time) string {
	d := until.Sub(now)
	if d <= 0 {
		return "retry shortly"
	}
	m := int((d + time.Minute - 1) / time.Minute)
	if m >= 60 {
		if r := m % 60; r != 0 {
			return fmt.Sprintf("retry in %dh %dm", m/60, r)
		}
		return fmt.Sprintf("retry in %dh", m/60)
	}
	return fmt.Sprintf("retry in %dm", m)
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
			Result:               resultLabel(o.Result),
			ResultTierClass:      resultTierClass(o.Result),
			ResultAria:           resultAria(o.Result),
			Detail:               o.Detail,
			Lane:                 laneLabel(o.ProviderLane),
			LaneMark:             laneMark(o.ProviderLane),
			CompletedAt:          display,
			CompletedAtISO:       iso,
			CompletedAtTZApplied: tzApplied,
		})
	}
	return rows
}

// stampRecentRelative fills the dashboard-only CompletedAtRelative label on
// rows built by buildRecentRows (#1263). It is separate so the Reports table,
// which shares the row type, keeps its absolute timestamp.
func stampRecentRelative(rows []templates.RecentOutcomeRow, recent []reports.RecentOutcome, now time.Time) {
	for i := range rows {
		if i < len(recent) {
			rows[i].CompletedAtRelative = formatRelativeTime(recent[i].CompletedAt, now)
		}
	}
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

// buildAttentionRows shapes reports.NeedsAttention rows for the dashboard and
// Reports (#654 AC2). A failed row names its failsig class in text, so the
// state never rests on color alone; a deferred row is not classified.
// updated_at is RFC3339 (the trigger's format); an unparsable or empty value
// renders "-", like a NULL completion time.
func buildAttentionRows(items []reports.FailureItem, loc *time.Location) []templates.AttentionRow {
	rows := make([]templates.AttentionRow, 0, len(items))
	for _, it := range items {
		state, class := "deferred", "mx-result-tier mx-result-tier-deferred"
		if it.Status == "failed" {
			state, class = "failed ("+it.Class.String()+")", "mx-result-tier mx-result-tier-failed"
		}
		at, _ := time.Parse(time.RFC3339, it.UpdatedAt)
		rows = append(rows, templates.AttentionRow{
			Artist: it.Artist, Album: it.Album, Title: it.Title,
			State: state, StateClass: class, Reason: it.Reason,
			LastAttempt: formatReportTime(at, loc),
		})
	}
	return rows
}
