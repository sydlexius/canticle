package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// blockingRealigner signals entry and blocks until released.
type blockingRealigner struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingRealigner) RealignDir(_ context.Context, _ string) error {
	b.calls.Add(1)
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return nil
}

func lidarrRenameFixture(t *testing.T) (root, body string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	dir := filepath.Join(root, "Artist", "Album")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "01. track.flac")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return root, `{"eventType":"Rename","artist":{"artistName":"Artist"},"album":{"title":"Album"},` +
		`"renamedTrackFiles":[{"previousPath":"` + path + `","path":"` + path + `"}]}`
}

func postLidarr(h *Handler, body string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(body)))
	return rec.Code
}

// TestCloseDropsRealignHeldAtTheGate pins the #1138 shutdown race: a realign
// held at the startup gate is dropped, never run, when shutdown is signaled
// first, even if the gate then opens (the gate is closed by runServe on the
// way out), and Close returns.
func TestCloseDropsRealignHeldAtTheGate(t *testing.T) {
	root, body := lidarrRenameFixture(t)
	gate := make(chan struct{})
	fr := &fakeRealigner{}
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", WithAllowedRoots([]string{root}), WithRealigner(fr), WithStartupGate(gate))
	if code := postLidarr(h, body); code != http.StatusOK {
		t.Fatalf("webhook status = %d; want 200", code)
	}
	closed := make(chan struct{})
	go func() { h.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while a realign was held at the gate")
	}
	close(gate) // shutdown closes the gate after the fact; the dropped pass must stay dropped
	h.bgRealign.Wait()
	if len(fr.dirs) != 0 {
		t.Errorf("realign ran after shutdown was signaled: %v", fr.dirs)
	}
}

// TestCloseWaitsForInFlightRealign pins that shutdown drains a realign that is
// already running (dispatched after the gate) before returning, so the caller
// can close the database safely.
func TestCloseWaitsForInFlightRealign(t *testing.T) {
	root, body := lidarrRenameFixture(t)
	br := &blockingRealigner{entered: make(chan struct{}, 1), release: make(chan struct{})}
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", WithAllowedRoots([]string{root}), WithRealigner(br))
	if code := postLidarr(h, body); code != http.StatusOK {
		t.Fatalf("webhook status = %d; want 200", code)
	}
	select {
	case <-br.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("realign never started")
	}
	closed := make(chan struct{})
	go func() { h.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a realign was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(br.release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the realign finished")
	}
}

// TestLidarrWebhookDefersRealignUntilStartupGateCloses pins #1138: while the
// one-time startup backfill is running, the webhook still answers 200 and
// enqueues (a queue row writes no lyric file), but the realign that renames
// lyric files waits for the gate, then runs. /api/v1/status reports the pass
// as running meanwhile.
func TestLidarrWebhookDefersRealignUntilStartupGateCloses(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	dir := filepath.Join(root, "Artist", "Album")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "01. track.flac")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	gate := make(chan struct{})
	fr := &fakeRealigner{}
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", WithAllowedRoots([]string{root}), WithRealigner(fr), WithStartupGate(gate))
	body := `{"eventType":"Rename","artist":{"artistName":"Artist"},"album":{"title":"Album"},` +
		`"renamedTrackFiles":[{"previousPath":"` + path + `","path":"` + path + `"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status = %d; want 200 (deferred, not refused)", rec.Code)
	}
	srec := httptest.NewRecorder()
	h.ServeHTTP(srec, httptest.NewRequest(http.MethodGet, "/api/v1/status?apikey=key", http.NoBody))
	if !strings.Contains(srec.Body.String(), `"startup_backfill":"running"`) {
		t.Errorf("status body %q lacks startup_backfill=running", srec.Body.String())
	}

	// Give the detached goroutine ample time to (wrongly) run ahead of the gate.
	time.Sleep(100 * time.Millisecond)
	if len(fr.dirs) != 0 {
		t.Fatalf("realign ran before the gate closed: %v", fr.dirs)
	}
	close(gate)
	h.bgRealign.Wait()
	if len(fr.dirs) == 0 {
		t.Error("realign never ran after the gate closed")
	}
	srec = httptest.NewRecorder()
	h.ServeHTTP(srec, httptest.NewRequest(http.MethodGet, "/api/v1/status?apikey=key", http.NoBody))
	if strings.Contains(srec.Body.String(), "startup_backfill") {
		t.Errorf("status body %q still reports the backfill after the gate closed", srec.Body.String())
	}
}
