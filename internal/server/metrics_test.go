package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/auth"
)

type fakeMetrics struct {
	statusCounts  map[string]int64
	failureCounts map[string]int64
	statusErr     error
	failureErr    error
}

func (f *fakeMetrics) CountByStatus(_ context.Context) (map[string]int64, error) {
	return f.statusCounts, f.statusErr
}

func (f *fakeMetrics) CountFailuresByReason(_ context.Context) (map[string]int64, error) {
	return f.failureCounts, f.failureErr
}

func TestMetricsRequiresAdminAuth(t *testing.T) {
	t.Run("no api key returns 401", func(t *testing.T) {
		h := NewHandler(&fakeAuth{err: auth.ErrInvalidKey}, &fakeQueue{}, "lyrics",
			WithMetricsReporter(&fakeMetrics{}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?apikey=bad", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d; want 401", rec.Code)
		}
	})

	t.Run("webhook-scoped key returns 403", func(t *testing.T) {
		h := NewHandler(&fakeAuth{err: auth.ErrForbiddenScope}, &fakeQueue{}, "lyrics",
			WithMetricsReporter(&fakeMetrics{}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?apikey=webhook-key", nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d; want 403", rec.Code)
		}
	})

	t.Run("auth backend error returns 500", func(t *testing.T) {
		h := NewHandler(&fakeAuth{err: errors.New("auth store down")}, &fakeQueue{}, "lyrics",
			WithMetricsReporter(&fakeMetrics{}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?apikey=key", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; want 500", rec.Code)
		}
	})

	t.Run("valid admin key passes auth gate", func(t *testing.T) {
		a := &fakeAuth{}
		h := NewHandler(a, &fakeQueue{}, "lyrics",
			WithMetricsReporter(&fakeMetrics{
				statusCounts:  map[string]int64{},
				failureCounts: map[string]int64{},
			}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?apikey=admin", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if a.required != auth.ScopeAdmin {
			t.Fatalf("required scope = %q; want admin", a.required)
		}
	})
}

func TestMetricsWithoutReporterReturns500(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics") // no WithMetricsReporter
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?apikey=key", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 when no reporter configured", rec.Code)
	}
}

func TestMetricsResponseIsValidPrometheusFormat(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{
			statusCounts:  map[string]int64{"pending": 5, "done": 12, "failed": 2},
			failureCounts: map[string]int64{"connection refused": 2},
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?apikey=key", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %q)", rec.Code, rec.Body.String())
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type = %q; want text/plain", ct)
	}

	body := rec.Body.String()

	// Metric family: queue items.
	if !strings.Contains(body, "# HELP mxlrcgo_queue_items") {
		t.Errorf("missing HELP line for mxlrcgo_queue_items\nbody:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE mxlrcgo_queue_items gauge") {
		t.Errorf("missing TYPE gauge line for mxlrcgo_queue_items\nbody:\n%s", body)
	}
	if !strings.Contains(body, `mxlrcgo_queue_items{status="pending"} 5`) {
		t.Errorf("missing pending sample\nbody:\n%s", body)
	}
	if !strings.Contains(body, `mxlrcgo_queue_items{status="done"} 12`) {
		t.Errorf("missing done sample\nbody:\n%s", body)
	}
	if !strings.Contains(body, `mxlrcgo_queue_items{status="failed"} 2`) {
		t.Errorf("missing failed sample\nbody:\n%s", body)
	}

	// Metric family: failures.
	if !strings.Contains(body, "# HELP mxlrcgo_failures_total") {
		t.Errorf("missing HELP line for mxlrcgo_failures_total\nbody:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE mxlrcgo_failures_total gauge") {
		t.Errorf("missing TYPE gauge line for mxlrcgo_failures_total\nbody:\n%s", body)
	}
	if !strings.Contains(body, `mxlrcgo_failures_total{reason="connection refused"} 2`) {
		t.Errorf("missing failure sample\nbody:\n%s", body)
	}
}

func TestMetricsEmptyQueueProducesHelpAndTypeLines(t *testing.T) {
	// No items in the queue: HELP/TYPE lines must still appear, but no samples.
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{
			statusCounts:  map[string]int64{},
			failureCounts: map[string]int64{},
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?apikey=key", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "# HELP mxlrcgo_queue_items") {
		t.Errorf("missing HELP for mxlrcgo_queue_items\nbody:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE mxlrcgo_queue_items gauge") {
		t.Errorf("missing TYPE for mxlrcgo_queue_items\nbody:\n%s", body)
	}
	if !strings.Contains(body, "# HELP mxlrcgo_failures_total") {
		t.Errorf("missing HELP for mxlrcgo_failures_total\nbody:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE mxlrcgo_failures_total gauge") {
		t.Errorf("missing TYPE for mxlrcgo_failures_total\nbody:\n%s", body)
	}
}

func TestMetricsStatusQueryErrorReturns500(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{statusErr: errors.New("db error")}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?apikey=key", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 on query error", rec.Code)
	}
}

func TestMetricsFailureQueryErrorReturns500(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{
			statusCounts: map[string]int64{"pending": 1},
			failureErr:   errors.New("db error"),
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?apikey=key", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 on query error", rec.Code)
	}
}

func TestWriteMetricsLabelEscaping(t *testing.T) {
	// Verify promEscape handles the characters mandated by the Prometheus spec.
	cases := []struct {
		input string
		want  string
	}{
		{`normal`, `normal`},
		{`has "quotes"`, `has \"quotes\"`},
		{`back\slash`, `back\\slash`},
		{"new\nline", `new\nline`},
	}
	for _, tc := range cases {
		got := promEscape(tc.input)
		if got != tc.want {
			t.Errorf("promEscape(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

func TestWriteMetricsSortedOutput(t *testing.T) {
	// Samples must appear in lexicographic order regardless of map iteration.
	var sb strings.Builder
	writeMetrics(&sb, map[string]int64{"pending": 1, "done": 2, "failed": 3}, map[string]int64{})
	body := sb.String()

	doneIdx := strings.Index(body, `status="done"`)
	failedIdx := strings.Index(body, `status="failed"`)
	pendingIdx := strings.Index(body, `status="pending"`)

	if doneIdx < 0 || failedIdx < 0 || pendingIdx < 0 {
		t.Fatalf("missing sample lines\nbody:\n%s", body)
	}
	if doneIdx >= failedIdx || failedIdx >= pendingIdx {
		t.Errorf("samples not in sorted order (done=%d failed=%d pending=%d)\nbody:\n%s",
			doneIdx, failedIdx, pendingIdx, body)
	}
}
