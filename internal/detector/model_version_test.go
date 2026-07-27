package detector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newVersionDetector builds a detector pointed at srv with the minimum config the
// model-version path needs.
func newVersionDetector(t *testing.T, url string) *HTTPDetector {
	t.Helper()
	d, err := NewHTTPDetector(Config{
		ClassifierURL:         url,
		SampleDurationSeconds: 30,
		MinConfidence:         0.90,
		InstrumentalClasses:   []string{"Music"},
		VocalClasses:          []string{"Singing"},
		VocalMaxConfidence:    0.05,
		FFmpegPath:            fakeFFmpeg(t),
		Version:               "9.9.9-app",
	})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}
	return d
}

func healthServer(t *testing.T, body map[string]any, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q; want /health", r.URL.Path)
		}
		if hits != nil {
			hits.Add(1)
		}
		if body == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
}

// The core of #684: the cache key must identify the MODEL, not the app build.
// Keying on the app version invalidated every stored verdict on every release,
// so the detector re-ran inference across the whole backlog and the library
// disks never idled.
func TestModelVersionComesFromSidecarNotAppVersion(t *testing.T) {
	srv := healthServer(t, map[string]any{"status": "ok", "model_version": "sha-abc"}, nil)
	defer srv.Close()

	d := newVersionDetector(t, srv.URL)

	if got := d.ModelVersion(); got != "sha-abc" {
		t.Fatalf("ModelVersion() = %q; want the SIDECAR model version %q", got, "sha-abc")
	}
	if got := d.ModelVersion(); got == "9.9.9-app" {
		t.Fatal("ModelVersion() returned the APP version; that is the #684 defect")
	}
}

// An app-version bump must NOT invalidate the cache: two detectors built from
// different app versions against the same sidecar must agree on the key.
func TestModelVersionSurvivesAppVersionBump(t *testing.T) {
	srv := healthServer(t, map[string]any{"status": "ok", "model_version": "sha-abc"}, nil)
	defer srv.Close()

	older := newVersionDetector(t, srv.URL)
	newer, err := NewHTTPDetector(Config{
		ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90,
		InstrumentalClasses: []string{"Music"}, VocalClasses: []string{"Singing"},
		VocalMaxConfidence: 0.05, FFmpegPath: fakeFFmpeg(t), Version: "10.0.0-app",
	})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	if older.ModelVersion() != newer.ModelVersion() {
		t.Fatalf("app-version bump changed the cache key (%q -> %q); stored verdicts would all be discarded",
			older.ModelVersion(), newer.ModelVersion())
	}
}

// An older sidecar that does not report a version must yield UNKNOWN, which
// fails closed to re-inference rather than reusing verdicts blindly.
func TestModelVersionUnknownWhenSidecarOmitsIt(t *testing.T) {
	srv := healthServer(t, map[string]any{"status": "ok", "classes": 521}, nil)
	defer srv.Close()

	if got := newVersionDetector(t, srv.URL).ModelVersion(); got != "" {
		t.Fatalf("ModelVersion() = %q; want \"\" (unknown) for a sidecar with no model_version", got)
	}
}

// A version learned once is cached: the probe must not run per work item.
func TestModelVersionProbesOnceThenCaches(t *testing.T) {
	var hits atomic.Int32
	srv := healthServer(t, map[string]any{"status": "ok", "model_version": "sha-abc"}, &hits)
	defer srv.Close()

	d := newVersionDetector(t, srv.URL)
	for range 5 {
		if got := d.ModelVersion(); got != "sha-abc" {
			t.Fatalf("ModelVersion() = %q; want sha-abc", got)
		}
	}

	if n := hits.Load(); n != 1 {
		t.Fatalf("health probed %d times; want exactly 1 (the version must be cached, not fetched per item)", n)
	}
}

// The boot race (#567): the sidecar is routinely still starting when canticle
// comes up. A failed probe must NOT be cached as empty -- that would disable the
// verdict cache for the whole process lifetime and silently reproduce #684.
func TestModelVersionRecoversAfterSidecarBoots(t *testing.T) {
	var booted atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !booted.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "model_version": "sha-abc"})
	}))
	defer srv.Close()

	d := newVersionDetector(t, srv.URL)

	if got := d.ModelVersion(); got != "" {
		t.Fatalf("ModelVersion() = %q while the sidecar is booting; want \"\"", got)
	}

	booted.Store(true)
	// Clear the retry throttle to simulate the interval elapsing.
	d.modelVersionMu.Lock()
	d.modelVersionNext = time.Time{}
	d.modelVersionMu.Unlock()

	if got := d.ModelVersion(); got != "sha-abc" {
		t.Fatalf("ModelVersion() = %q after the sidecar booted; want sha-abc. "+
			"A failed probe must not be cached as permanently unknown", got)
	}
}

// A failing sidecar must not be probed once per call; retries are throttled.
func TestModelVersionThrottlesRetriesWhileUnknown(t *testing.T) {
	var hits atomic.Int32
	srv := healthServer(t, nil, &hits) // always 503
	defer srv.Close()

	d := newVersionDetector(t, srv.URL)
	for range 5 {
		if got := d.ModelVersion(); got != "" {
			t.Fatalf("ModelVersion() = %q; want \"\" while the sidecar is failing", got)
		}
	}

	if n := hits.Load(); n != 1 {
		t.Fatalf("health probed %d times; want 1 (retries must be throttled, not per-call)", n)
	}
}

// Guard the context plumbing: the probe must respect a deadline rather than hang.
func TestModelVersionProbeRespectsContext(t *testing.T) {
	srv := healthServer(t, map[string]any{"status": "ok", "model_version": "sha-abc"}, nil)
	defer srv.Close()

	d := newVersionDetector(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := d.resolveModelVersion(ctx); got != "" {
		t.Fatalf("resolveModelVersion(canceled ctx) = %q; want \"\"", got)
	}
}

// A sidecar MODEL SWAP must be picked up without restarting canticle.
//
// The cache used to hold the first non-empty version for the whole process
// lifetime, so after an operator redeployed the sidecar with new weights the
// detector kept stamping the OLD hash onto verdicts the NEW model produced.
// That is worse than stale: it attributes a score to weights that did not
// compute it, and it makes those scores reusable across a real model change --
// exactly what the key exists to prevent (#684).
func TestModelVersionRefreshesAfterModelSwap(t *testing.T) {
	var current atomic.Value
	current.Store("model-A")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "model_version": current.Load().(string),
		})
	}))
	defer srv.Close()

	d := newVersionDetector(t, srv.URL)

	if got := d.ModelVersion(); got != "model-A" {
		t.Fatalf("ModelVersion() = %q; want model-A", got)
	}

	// Operator redeploys the sidecar with different weights.
	current.Store("model-B")

	// Within the TTL the cached value is still served -- that is the point of
	// the cache, and re-probing per call would defeat it.
	if got := d.ModelVersion(); got != "model-A" {
		t.Fatalf("ModelVersion() = %q inside the TTL; want the cached model-A", got)
	}

	// Once the TTL lapses the next call must see the NEW model.
	d.modelVersionMu.Lock()
	d.modelVersionNext = time.Now().Add(-time.Second)
	d.modelVersionMu.Unlock()

	if got := d.ModelVersion(); got != "model-B" {
		t.Fatalf("ModelVersion() = %q after the TTL lapsed; want model-B. "+
			"A swapped sidecar must not keep stamping the previous model's hash", got)
	}
}

// A probe failure AFTER a version is known must keep the last known value, not
// revert to unknown. Unknown fails closed to re-inference, so a transient blip
// would otherwise re-infer the entire backlog -- the #684 symptom, triggered by
// one dropped request.
func TestModelVersionKeepsLastKnownOnTransientFailure(t *testing.T) {
	var up atomic.Bool
	up.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "model_version": "model-A"})
	}))
	defer srv.Close()

	d := newVersionDetector(t, srv.URL)
	if got := d.ModelVersion(); got != "model-A" {
		t.Fatalf("ModelVersion() = %q; want model-A", got)
	}

	up.Store(false) // sidecar goes down
	d.modelVersionMu.Lock()
	d.modelVersionNext = time.Now().Add(-time.Second)
	d.modelVersionMu.Unlock()

	if got := d.ModelVersion(); got != "model-A" {
		t.Fatalf("ModelVersion() = %q after a failed refresh; want the last known model-A. "+
			"Reverting to unknown would re-infer the whole backlog on a transient blip", got)
	}
}
