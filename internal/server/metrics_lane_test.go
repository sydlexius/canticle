package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/orchestrator"
)

func scrapeMetrics(t *testing.T, opts ...Option) string {
	t.Helper()
	opts = append([]Option{WithMetricsReporter(&fakeMetrics{
		statusCounts:  map[string]int64{},
		failureCounts: map[string]int64{},
	})}, opts...)
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", opts...)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	return rec.Body.String()
}

// TestMetricsLaneFamilies verifies the per-lane gauges for an open and a closed
// lane (#488): one-hot state, open-until only while open, trips, lane label only.
func TestMetricsLaneFamilies(t *testing.T) {
	until := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	body := scrapeMetrics(t, WithLaneHealth(func() []orchestrator.LaneState {
		return []orchestrator.LaneState{
			{Provider: "musixmatch", State: orchestrator.LaneStateOpen, OpenUntil: until, Trips: 3},
			{Provider: "petitlyrics", State: orchestrator.LaneStateClosed},
		}
	}))
	for _, want := range []string{
		"# TYPE mxlrcgo_lane_state gauge\n",
		`mxlrcgo_lane_state{lane="musixmatch",state="open"} 1`,
		`mxlrcgo_lane_state{lane="musixmatch",state="closed"} 0`,
		`mxlrcgo_lane_state{lane="musixmatch",state="half-open"} 0`,
		`mxlrcgo_lane_state{lane="petitlyrics",state="closed"} 1`,
		`mxlrcgo_lane_state{lane="petitlyrics",state="open"} 0`,
		`mxlrcgo_lane_open_until_timestamp_seconds{lane="musixmatch"} ` + strconv.FormatInt(until.Unix(), 10),
		`mxlrcgo_lane_open_until_timestamp_seconds{lane="petitlyrics"} 0`,
		`mxlrcgo_lane_trips{lane="musixmatch"} 3`,
		`mxlrcgo_lane_trips{lane="petitlyrics"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
}

// TestMetricsLaneHalfOpenAndStaleOpenUntil verifies a half-open lane reports
// state half-open and a zero open-until even if the snapshot carried a time, and
// that the lane label is escaped.
func TestMetricsLaneHalfOpenAndStaleOpenUntil(t *testing.T) {
	body := scrapeMetrics(t, WithLaneHealth(func() []orchestrator.LaneState {
		return []orchestrator.LaneState{{Provider: "a\"b", State: orchestrator.LaneStateHalfOpen, OpenUntil: time.Unix(99, 0)}}
	}))
	for _, want := range []string{
		`mxlrcgo_lane_state{lane="a\"b",state="half-open"} 1`,
		`mxlrcgo_lane_open_until_timestamp_seconds{lane="a\"b"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
}

// TestMetricsNoLaneSeamOmitsFamilies verifies an unwired handler emits no lane
// families and does not panic.
func TestMetricsNoLaneSeamOmitsFamilies(t *testing.T) {
	if body := scrapeMetrics(t); strings.Contains(body, "mxlrcgo_lane_") {
		t.Errorf("no seam must emit no lane families\n%s", body)
	}
}

// TestMetricsLaneSeamReadPerScrape verifies the seam is called on every scrape,
// so a source that swaps its lanes (a rebuild) is reflected on the next scrape.
func TestMetricsLaneSeamReadPerScrape(t *testing.T) {
	lanes := []orchestrator.LaneState{{Provider: "old", State: orchestrator.LaneStateClosed}}
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{statusCounts: map[string]int64{}, failureCounts: map[string]int64{}}),
		WithLaneHealth(func() []orchestrator.LaneState { return lanes }))
	scrape := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, metricsRequest())
		return rec.Body.String()
	}
	if !strings.Contains(scrape(), `lane="old"`) {
		t.Fatal("first scrape missing old lane")
	}
	lanes = []orchestrator.LaneState{{Provider: "new", State: orchestrator.LaneStateClosed}}
	body := scrape()
	if !strings.Contains(body, `lane="new"`) || strings.Contains(body, `lane="old"`) {
		t.Errorf("scrape after swap must show only the new lanes\n%s", body)
	}
}

// TestMetricsLaneFamiliesStayTrustGated verifies the lane families do not widen
// access: an untrusted client gets 403 and no lane data.
func TestMetricsLaneFamiliesStayTrustGated(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{statusCounts: map[string]int64{}, failureCounts: map[string]int64{}}),
		WithLaneHealth(func() []orchestrator.LaneState {
			return []orchestrator.LaneState{{Provider: "musixmatch", State: orchestrator.LaneStateClosed}}
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, remoteMetricsRequest("203.0.113.7:5555", ""))
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "mxlrcgo_lane_") {
		t.Fatalf("status = %d body %q; want 403 and no lane data", rec.Code, rec.Body.String())
	}
}
