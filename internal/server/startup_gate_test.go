package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
