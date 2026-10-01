package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/reports"
)

func laneHealthTestUI(t *testing.T, fn func() []orchestrator.LaneState) *http.ServeMux {
	t.Helper()
	sqlDB := openReportsTestDB(t)
	for i, lane := range []string{"musixmatch", "petitlyrics", "innertube"} {
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

func getDashboard(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestDashboard_LaneStatus(t *testing.T) {
	health := []orchestrator.LaneState{
		{Provider: "musixmatch", State: orchestrator.LaneStateClosed},
		{Provider: "petitlyrics", State: orchestrator.LaneStateHalfOpen},
		{Provider: "innertube", State: orchestrator.LaneStateOpen, OpenUntil: time.Now().Add(4*time.Minute + 20*time.Second)},
	}
	body := getDashboard(t, laneHealthTestUI(t, func() []orchestrator.LaneState { return health }))
	for _, want := range []string{
		`mx-dash-tile-status-healthy">Healthy<`,
		`mx-dash-tile-status-probing">Probing<`,
		`mx-dash-tile-status-throttled">Throttled, retry in 4m<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

func TestDashboard_NoLaneHealthSeam(t *testing.T) {
	body := getDashboard(t, laneHealthTestUI(t, nil))
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
	mux := laneHealthTestUI(t, func() []orchestrator.LaneState {
		return []orchestrator.LaneState{{Provider: "musixmatch", State: state}}
	})
	if body := getDashboard(t, mux); !strings.Contains(body, ">Healthy<") || strings.Contains(body, ">Probing<") {
		t.Fatal("first render should be healthy")
	}
	state = orchestrator.LaneStateHalfOpen
	if body := getDashboard(t, mux); !strings.Contains(body, ">Probing<") {
		t.Error("second render did not reflect the swapped lane state")
	}
}

func TestRetryIn(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "retry shortly"},
		{30 * time.Second, "retry in <1m"},
		{4*time.Minute + 20*time.Second, "retry in 4m"},
		{60 * time.Minute, "retry in 1h"},
		{65*time.Minute + 5*time.Second, "retry in 1h 5m"},
	} {
		if got := retryIn(now.Add(tc.d), now); got != tc.want {
			t.Errorf("retryIn(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
