package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/detectorbackfill"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/reports"
)

// laneHealthTestUI mounts a dashboard over a reports DB holding one hit for
// each of lanes (none when lanes is empty) and, when fn is non-nil, fn as the
// lane-health source.
func laneHealthTestUI(t *testing.T, lanes []string, fn func() []orchestrator.LaneState) *http.ServeMux {
	t.Helper()
	sqlDB := openReportsTestDB(t)
	for i, lane := range lanes {
		if _, err := sqlDB.ExecContext(t.Context(),
			`INSERT INTO lane_attempts (queue_id, lane, hit, attempted_at) VALUES (?, ?, 1, '2026-06-18T00:00:00Z')`,
			int64(i+1), lane); err != nil {
			t.Fatalf("insert lane_attempts: %v", err)
		}
	}
	mux := http.NewServeMux()
	ui := NewUI(config.Config{}, "v-test", WithReports(reports.New(sqlDB)))
	if fn != nil {
		ui.AttachLaneHealth(fn)
	}
	ui.Register(mux)
	return mux
}

var threeLanes = []string{"musixmatch", "petitlyrics", "innertube"}

func getDashboard(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func statusSpan(class, text string) string {
	return `mx-dash-tile-status-` + class + `">` + text + `<`
}

func TestDashboard_LaneStatus(t *testing.T) {
	health := []orchestrator.LaneState{
		{Provider: "musixmatch", State: orchestrator.LaneStateClosed, EverSucceeded: true},
		{Provider: "petitlyrics", State: orchestrator.LaneStateHalfOpen, EverSucceeded: true},
		{Provider: "innertube", State: orchestrator.LaneStateOpen, EverSucceeded: true, OpenUntil: time.Now().Add(4*time.Minute + 20*time.Second)},
	}
	body := getDashboard(t, laneHealthTestUI(t, threeLanes, func() []orchestrator.LaneState { return health }))
	for _, want := range []string{
		statusSpan("healthy", "Healthy"),
		statusSpan("probing", "Probing"),
		statusSpan("throttled", "Throttled, retry in 5m"),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

// TestDashboard_NeverSucceededLanes covers the lanes that have not resolved
// once this session: closed reads "Ready, no success this session" (nothing has proven
// it healthy) and open reads "Failing ... check token/config", never
// "Throttled" -- the verify-your-token case.
func TestDashboard_NeverSucceededLanes(t *testing.T) {
	health := []orchestrator.LaneState{
		{Provider: "musixmatch", State: orchestrator.LaneStateOpen, OpenUntil: time.Now().Add(10 * time.Minute)},
		{Provider: "petitlyrics", State: orchestrator.LaneStateClosed},
	}
	body := getDashboard(t, laneHealthTestUI(t, threeLanes, func() []orchestrator.LaneState { return health }))
	for _, want := range []string{
		statusSpan("failing", "Failing, no success this session - check token/config (retry in 10m)"),
		statusSpan("ready", "Ready, no success this session"),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "Throttled") || strings.Contains(body, ">Healthy<") {
		t.Error("a never-succeeded lane must not read Throttled or Healthy")
	}
}

// A lane opened by a provider refusal (HTTP 403) reads as refused, never as
// throttled or as a token problem (#1372).
func TestDashboard_RefusedLane(t *testing.T) {
	health := []orchestrator.LaneState{
		{Provider: "musixmatch", State: orchestrator.LaneStateOpen, Refused: true, EverSucceeded: true, OpenUntil: time.Now().Add(10 * time.Minute)},
	}
	body := getDashboard(t, laneHealthTestUI(t, threeLanes, func() []orchestrator.LaneState { return health }))
	if want := statusSpan("failing", "Refused by the provider (HTTP 403), not throttling (retry in 10m)"); !strings.Contains(body, want) {
		t.Errorf("dashboard missing %q", want)
	}
	if strings.Contains(body, "Throttled") || strings.Contains(body, "check token") {
		t.Error("a refused lane must not read Throttled or as a token problem")
	}
}

// A refused lane being re-probed (half-open) still reads as refused, not as a
// neutral "Probing": the refusal stands until a probe is answered (#1372).
func TestDashboard_RefusedLaneHalfOpen(t *testing.T) {
	health := []orchestrator.LaneState{
		{Provider: "musixmatch", State: orchestrator.LaneStateHalfOpen, Refused: true, EverSucceeded: true},
		{Provider: "petitlyrics", State: orchestrator.LaneStateHalfOpen, EverSucceeded: true},
	}
	body := getDashboard(t, laneHealthTestUI(t, threeLanes, func() []orchestrator.LaneState { return health }))
	if want := statusSpan("failing", "Refused by the provider (HTTP 403), probing again"); !strings.Contains(body, want) {
		t.Errorf("dashboard missing %q", want)
	}
	if got := strings.Count(body, statusSpan("probing", "Probing")); got != 1 {
		t.Errorf("plain Probing tiles = %d; want 1 (the lane that was not refused)", got)
	}
}

// TestDashboard_HealthOnlyLaneGetsTile is the fresh-install bad-token case: no
// lane_attempts rows at all, but the configured lane's breaker is open. It
// must still get a tile (zero counts) with its status, not the empty state.
func TestDashboard_HealthOnlyLaneGetsTile(t *testing.T) {
	health := []orchestrator.LaneState{
		{Provider: "musixmatch", State: orchestrator.LaneStateOpen, OpenUntil: time.Now().Add(10 * time.Minute)},
		{Provider: "detector", Local: true, State: orchestrator.LaneStateClosed},
	}
	body := getDashboard(t, laneHealthTestUI(t, nil, func() []orchestrator.LaneState { return health }))
	if strings.Contains(body, "No lyrics source data yet") {
		t.Error("a configured lane must suppress the empty state")
	}
	if !strings.Contains(body, statusSpan("failing", "Failing, no success this session - check token/config (retry in 10m)")) {
		t.Error("health-only lane missing its status line")
	}
	if !strings.Contains(body, `mx-dash-tile-value">0/0<`) {
		t.Error("health-only lane tile should render 0/0 counts")
	}
	if got := strings.Count(body, `class="mx-dash-tile-status `); got != 1 {
		t.Errorf("status lines = %d, want 1 (the Local detector lane gets none)", got)
	}
	// The Local detector has no history here, so it must get no tile at all.
	if strings.Contains(body, "Instrumental Detector") {
		t.Error("a Local lane with no recorded attempts must not get a tile")
	}
}

// TestDashboard_MusixmatchInactiveTile: with no token the worker never starts,
// so a closed never-succeeded musixmatch lane must read inactive, not Ready.
func TestDashboard_MusixmatchInactiveTile(t *testing.T) {
	health := []orchestrator.LaneState{
		{Provider: "musixmatch", State: orchestrator.LaneStateClosed},
	}
	sqlDB := openReportsTestDB(t)
	mux := http.NewServeMux()
	ui := NewUI(config.Config{}, "v-test", WithReports(reports.New(sqlDB)))
	ui.AttachLaneHealth(func() []orchestrator.LaneState { return health })
	ui.AttachMusixmatchInactive(true)
	ui.Register(mux)
	body := getDashboard(t, mux)
	if !strings.Contains(body, statusSpan("inactive", "Inactive - add an API token")) {
		t.Error("inactive musixmatch tile missing its inactive status")
	}
	if strings.Contains(body, "Ready") {
		t.Error("an inactive musixmatch lane must not read Ready")
	}
}

// TestDashboard_EmptyStateNeedsBothEmpty verifies the empty state still shows
// when neither health nor history names a lane.
func TestDashboard_EmptyStateNeedsBothEmpty(t *testing.T) {
	body := getDashboard(t, laneHealthTestUI(t, nil, func() []orchestrator.LaneState { return nil }))
	if !strings.Contains(body, "No lyrics source data yet") {
		t.Error("no lanes anywhere: empty state missing")
	}
}

// TestDashboard_UnconfiguredLaneNotActive verifies a lane with history but no
// health entry (removed from config) reads "Not active", ordered after the
// configured lanes; a Local lane with history keeps its tile but no status.
func TestDashboard_UnconfiguredLaneNotActive(t *testing.T) {
	health := []orchestrator.LaneState{
		{Provider: "innertube", State: orchestrator.LaneStateClosed, EverSucceeded: true},
		{Provider: "detector", Local: true, State: orchestrator.LaneStateClosed},
	}
	body := getDashboard(t, laneHealthTestUI(t, append([]string{"detector"}, threeLanes...),
		func() []orchestrator.LaneState { return health }))
	if got := strings.Count(body, statusSpan("inactive", "Not active")); got != 2 {
		t.Errorf("Not active lines = %d, want 2 (musixmatch, petitlyrics)", got)
	}
	if got := strings.Count(body, `class="mx-dash-tile-status `); got != 3 {
		t.Errorf("status lines = %d, want 3 (detector has none)", got)
	}
	if strings.Index(body, statusSpan("healthy", "Healthy")) > strings.Index(body, statusSpan("inactive", "Not active")) {
		t.Error("configured lanes must come before unconfigured ones")
	}
}

// TestDashboard_DetectorHistoryWithoutHealth: with the detector off or in
// parallel mode there is no detector health entry, but its recorded history
// still renders a tile, and that tile gets no status line (it is not a lyrics
// source). A history-only provider lane in the same render still reads
// "Not active".
func TestDashboard_DetectorHistoryWithoutHealth(t *testing.T) {
	health := []orchestrator.LaneState{
		{Provider: "innertube", State: orchestrator.LaneStateClosed, EverSucceeded: true},
	}
	body := getDashboard(t, laneHealthTestUI(t, []string{detectorbackfill.LaneName, "petitlyrics", "innertube"},
		func() []orchestrator.LaneState { return health }))
	if !strings.Contains(body, "Instrumental Detector") {
		t.Fatal("detector history must still render a tile")
	}
	if got := strings.Count(body, statusSpan("inactive", "Not active")); got != 1 {
		t.Errorf("Not active lines = %d, want 1 (petitlyrics only, never the detector)", got)
	}
	if got := strings.Count(body, `class="mx-dash-tile-status `); got != 2 {
		t.Errorf("status lines = %d, want 2 (innertube, petitlyrics; detector has none)", got)
	}
}

func TestDashboard_NoLaneHealthSeam(t *testing.T) {
	body := getDashboard(t, laneHealthTestUI(t, threeLanes, nil))
	if strings.Contains(body, "mx-dash-tile-status") {
		t.Error("no seam: tiles must render without a status line")
	}
	if !strings.Contains(body, "musixmatch") {
		t.Error("no seam: provider tiles missing")
	}
}

// TestDashboard_LaneHealthReread proves the source is read per request: a swap
// of what the func returns (as a worker rebuild does) shows on the next render.
func TestDashboard_LaneHealthReread(t *testing.T) {
	state := orchestrator.LaneStateClosed
	mux := laneHealthTestUI(t, threeLanes, func() []orchestrator.LaneState {
		return []orchestrator.LaneState{{Provider: "musixmatch", State: state, EverSucceeded: true}}
	})
	if body := getDashboard(t, mux); !strings.Contains(body, ">Healthy<") || strings.Contains(body, ">Probing<") {
		t.Fatal("first render should be healthy")
	}
	state = orchestrator.LaneStateHalfOpen
	if body := getDashboard(t, mux); !strings.Contains(body, ">Probing<") {
		t.Error("second render did not reflect the swapped lane state")
	}
}

// TestRetryIn pins the ceiling rounding: a countdown never promises a retry
// sooner than the window allows.
func TestRetryIn(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "retry shortly"},
		{0, "retry shortly"},
		{time.Second, "retry in 1m"},
		{30 * time.Second, "retry in 1m"},
		{time.Minute, "retry in 1m"},
		{61 * time.Second, "retry in 2m"},
		{90 * time.Second, "retry in 2m"},
		{4*time.Minute + 20*time.Second, "retry in 5m"},
		{59*time.Minute + 31*time.Second, "retry in 1h"},
		{60 * time.Minute, "retry in 1h"},
		{65*time.Minute + 5*time.Second, "retry in 1h 6m"},
	} {
		if got := retryIn(now.Add(tc.d), now); got != tc.want {
			t.Errorf("retryIn(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
