package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/trustnet"
)

// metricsRequest builds a GET /metrics request from loopback, which is
// implicitly trusted by the trusted-network gate so the content/format tests
// reach the handler body. No API key is needed: /metrics is gated by client IP,
// not by auth (issue #204, S3).
func metricsRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.RemoteAddr = "127.0.0.1:43210"
	return r
}

// remoteMetricsRequest builds a GET /metrics request from a non-loopback IP,
// optionally carrying an X-Forwarded-For header.
func remoteMetricsRequest(remoteAddr, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.RemoteAddr = remoteAddr
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

type fakeMetrics struct {
	statusCounts      map[string]int64
	failureCounts     map[string]int64
	providerHits      map[string]int64
	providerMisses    map[string]int64
	instrumentalCount int64
	statusErr         error
	failureErr        error
	hitsErr           error
	missesErr         error
	instrumentalErr   error
}

func (f *fakeMetrics) CountByStatus(_ context.Context) (map[string]int64, error) {
	return f.statusCounts, f.statusErr
}

func (f *fakeMetrics) CountFailuresByReason(_ context.Context) (map[string]int64, error) {
	return f.failureCounts, f.failureErr
}

func (f *fakeMetrics) ProviderHits(_ context.Context) (map[string]int64, error) {
	return f.providerHits, f.hitsErr
}

func (f *fakeMetrics) ProviderMisses(_ context.Context) (map[string]int64, error) {
	return f.providerMisses, f.missesErr
}

func (f *fakeMetrics) CountInstrumental(_ context.Context) (int64, error) {
	return f.instrumentalCount, f.instrumentalErr
}

// TestMetricsTrustedNetworkGate verifies that GET /metrics is gated by the
// trusted-network allowlist (issue #204, S3), not by an API key: loopback is
// implicitly trusted, configured CIDRs are trusted, everything else is refused,
// and a spoofed X-Forwarded-For cannot forge a trusted source.
func TestMetricsTrustedNetworkGate(t *testing.T) {
	okReporter := func() Option {
		return WithMetricsReporter(&fakeMetrics{
			statusCounts:  map[string]int64{},
			failureCounts: map[string]int64{},
		})
	}

	t.Run("loopback trusted with empty allowlist (default closed)", func(t *testing.T) {
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", okReporter())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, metricsRequest())
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body %q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-trusted IP refused with empty allowlist", func(t *testing.T) {
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", okReporter())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, remoteMetricsRequest("203.0.113.7:5555", ""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d; want 403", rec.Code)
		}
	})

	t.Run("in-allowlist IP trusted", func(t *testing.T) {
		p, err := trustnet.NewPolicy([]string{"192.168.0.0/16"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", okReporter(), WithTrustedNetworks(p))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, remoteMetricsRequest("192.168.5.5:5555", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body %q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("out-of-allowlist IP refused", func(t *testing.T) {
		p, err := trustnet.NewPolicy([]string{"192.168.0.0/16"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", okReporter(), WithTrustedNetworks(p))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, remoteMetricsRequest("203.0.113.7:5555", ""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d; want 403", rec.Code)
		}
	})

	t.Run("spoofed XFF cannot forge a trusted source", func(t *testing.T) {
		p, err := trustnet.NewPolicy([]string{"192.168.0.0/16"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", okReporter(), WithTrustedNetworks(p))
		rec := httptest.NewRecorder()
		// Direct (untrusted) peer claims an allowlisted IP via XFF; no trusted
		// proxy is configured, so XFF must be ignored and the request refused.
		h.ServeHTTP(rec, remoteMetricsRequest("203.0.113.7:5555", "192.168.5.5"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d; want 403 (spoofed XFF must not grant trust)", rec.Code)
		}
	})

	t.Run("real client behind trusted proxy is allowed", func(t *testing.T) {
		p, err := trustnet.NewPolicy([]string{"192.168.0.0/16"}, []string{"10.0.0.0/8"})
		if err != nil {
			t.Fatal(err)
		}
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", okReporter(), WithTrustedNetworks(p))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, remoteMetricsRequest("10.0.0.1:5555", "192.168.5.5"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body %q)", rec.Code, rec.Body.String())
		}
	})
}

func TestMetricsWithoutReporterReturns500(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics") // no WithMetricsReporter
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest())
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
	h.ServeHTTP(rec, metricsRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %q)", rec.Code, rec.Body.String())
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type = %q; want text/plain", ct)
	}

	cc := rec.Header().Get("Cache-Control")
	if cc != "no-store" {
		t.Fatalf("Cache-Control = %q; want no-store", cc)
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
	if !strings.Contains(body, "# HELP mxlrcgo_queue_failures") {
		t.Errorf("missing HELP line for mxlrcgo_queue_failures\nbody:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE mxlrcgo_queue_failures gauge") {
		t.Errorf("missing TYPE gauge line for mxlrcgo_queue_failures\nbody:\n%s", body)
	}
	if !strings.Contains(body, `mxlrcgo_queue_failures{reason="connection refused"} 2`) {
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
	h.ServeHTTP(rec, metricsRequest())

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
	if !strings.Contains(body, "# HELP mxlrcgo_queue_failures") {
		t.Errorf("missing HELP for mxlrcgo_queue_failures\nbody:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE mxlrcgo_queue_failures gauge") {
		t.Errorf("missing TYPE for mxlrcgo_queue_failures\nbody:\n%s", body)
	}
}

func TestMetricsStatusQueryErrorReturns500(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{statusErr: errors.New("db error")}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest())
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
	h.ServeHTTP(rec, metricsRequest())
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
	writeMetrics(&sb,
		map[string]int64{"pending": 1, "done": 2, "failed": 3},
		map[string]int64{},
		map[string]int64{},
		map[string]int64{},
		0,
	)
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

func TestMetricsProviderOutcomesAndInstrumental(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{
			statusCounts:      map[string]int64{"done": 10},
			failureCounts:     map[string]int64{},
			providerHits:      map[string]int64{"musixmatch": 8, "petitlyrics": 2},
			providerMisses:    map[string]int64{"musixmatch": 3},
			instrumentalCount: 5,
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Provider hits counter family.
	if !strings.Contains(body, "# HELP mxlrcgo_provider_hits_total") {
		t.Errorf("missing HELP for mxlrcgo_provider_hits_total\nbody:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE mxlrcgo_provider_hits_total counter") {
		t.Errorf("missing TYPE counter for mxlrcgo_provider_hits_total\nbody:\n%s", body)
	}
	if !strings.Contains(body, `mxlrcgo_provider_hits_total{lane="musixmatch"} 8`) {
		t.Errorf("missing musixmatch hit sample\nbody:\n%s", body)
	}
	if !strings.Contains(body, `mxlrcgo_provider_hits_total{lane="petitlyrics"} 2`) {
		t.Errorf("missing petitlyrics hit sample\nbody:\n%s", body)
	}

	// Provider misses counter family.
	if !strings.Contains(body, "# HELP mxlrcgo_provider_misses_total") {
		t.Errorf("missing HELP for mxlrcgo_provider_misses_total\nbody:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE mxlrcgo_provider_misses_total counter") {
		t.Errorf("missing TYPE counter for mxlrcgo_provider_misses_total\nbody:\n%s", body)
	}
	if !strings.Contains(body, `mxlrcgo_provider_misses_total{lane="musixmatch"} 3`) {
		t.Errorf("missing musixmatch miss sample\nbody:\n%s", body)
	}

	// Instrumental gauge.
	if !strings.Contains(body, "# HELP mxlrcgo_instrumental_tracks") {
		t.Errorf("missing HELP for mxlrcgo_instrumental_tracks\nbody:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE mxlrcgo_instrumental_tracks gauge") {
		t.Errorf("missing TYPE gauge for mxlrcgo_instrumental_tracks\nbody:\n%s", body)
	}
	if !strings.Contains(body, "mxlrcgo_instrumental_tracks 5") {
		t.Errorf("missing instrumental_tracks sample\nbody:\n%s", body)
	}

	// Sorted output within provider hits (musixmatch < petitlyrics).
	mmIdx := strings.Index(body, `lane="musixmatch"`)
	ptIdx := strings.Index(body, `lane="petitlyrics"`)
	if mmIdx < 0 || ptIdx < 0 {
		t.Fatalf("missing lane samples\nbody:\n%s", body)
	}
	if mmIdx >= ptIdx {
		t.Errorf("provider hits not in sorted lane order (musixmatch=%d petitlyrics=%d)\nbody:\n%s",
			mmIdx, ptIdx, body)
	}
}

func TestMetricsEmptyProviderTablesProduceHelpAndTypeLines(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{
			statusCounts:   map[string]int64{},
			failureCounts:  map[string]int64{},
			providerHits:   map[string]int64{},
			providerMisses: map[string]int64{},
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# HELP mxlrcgo_provider_hits_total",
		"# TYPE mxlrcgo_provider_hits_total counter",
		"# HELP mxlrcgo_provider_misses_total",
		"# TYPE mxlrcgo_provider_misses_total counter",
		"# HELP mxlrcgo_instrumental_tracks",
		"# TYPE mxlrcgo_instrumental_tracks gauge",
		"mxlrcgo_instrumental_tracks 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

func TestMetricsProviderHitsQueryErrorReturns500(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{
			statusCounts:  map[string]int64{},
			failureCounts: map[string]int64{},
			hitsErr:       errors.New("db error"),
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 on provider hits query error", rec.Code)
	}
}

func TestMetricsProviderMissesQueryErrorReturns500(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{
			statusCounts:  map[string]int64{},
			failureCounts: map[string]int64{},
			providerHits:  map[string]int64{},
			missesErr:     errors.New("db error"),
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 on provider misses query error", rec.Code)
	}
}

func TestMetricsInstrumentalQueryErrorReturns500(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{
			statusCounts:    map[string]int64{},
			failureCounts:   map[string]int64{},
			providerHits:    map[string]int64{},
			providerMisses:  map[string]int64{},
			instrumentalErr: errors.New("db error"),
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 on instrumental query error", rec.Code)
	}
}

// fakeCacheStatser is a test CacheStatser returning fixed hit/lookup counts.
type fakeCacheStatser struct{ hits, lookups int64 }

func (f fakeCacheStatser) CacheStats() (hits, lookups int64) { return f.hits, f.lookups }

// TestMetricsCacheCountersEmittedWithDecorator verifies that wrapping the base
// reporter with WithCacheStats adds the cache hit/lookup counter families to
// /metrics (the scalar-counter layout, mirroring mxlrcgo_instrumental_tracks).
func TestMetricsCacheCountersEmittedWithDecorator(t *testing.T) {
	base := &fakeMetrics{
		statusCounts:  map[string]int64{},
		failureCounts: map[string]int64{},
	}
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(WithCacheStats(base, fakeCacheStatser{hits: 7, lookups: 10})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# HELP mxlrcgo_cache_hits_total",
		"# TYPE mxlrcgo_cache_hits_total counter",
		"mxlrcgo_cache_hits_total 7",
		"# HELP mxlrcgo_cache_lookups_total",
		"# TYPE mxlrcgo_cache_lookups_total counter",
		"mxlrcgo_cache_lookups_total 10",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

// TestMetricsCacheCountersOmittedWithoutDecorator verifies that a bare reporter
// (no cache seam) omits the cache families rather than failing the scrape.
func TestMetricsCacheCountersOmittedWithoutDecorator(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithMetricsReporter(&fakeMetrics{
			statusCounts:  map[string]int64{},
			failureCounts: map[string]int64{},
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "mxlrcgo_cache_") {
		t.Error("bare reporter must not emit cache counter families")
	}
}

// TestWithCacheStatsForwardsBaseMetrics verifies the decorator forwards the
// embedded reporter's queue metrics in addition to adding cache stats.
func TestWithCacheStatsForwardsBaseMetrics(t *testing.T) {
	base := &fakeMetrics{statusCounts: map[string]int64{"done": 4}}
	dec := WithCacheStats(base, fakeCacheStatser{hits: 1, lookups: 2})
	got, err := dec.CountByStatus(context.Background())
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if got["done"] != 4 {
		t.Errorf("forwarded status count = %d, want 4", got["done"])
	}
}
