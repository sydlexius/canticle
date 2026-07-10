package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/doxazo-net/canticle/internal/auth"
	"github.com/doxazo-net/canticle/internal/models"
	"github.com/doxazo-net/canticle/internal/queue"
)

type fakeAuth struct {
	raw      string
	required auth.Scope
	err      error
}

func (f *fakeAuth) ValidateKey(_ context.Context, raw string, required auth.Scope) (auth.Key, error) {
	f.raw = raw
	f.required = required
	if f.err != nil {
		return auth.Key{}, f.err
	}
	return auth.Key{ID: "key"}, nil
}

type fakeQueue struct {
	items      []models.Inputs
	priorities []int
	cleanups   []models.Inputs
	err        error
}

func (f *fakeQueue) Enqueue(_ context.Context, inputs models.Inputs, priority int) (queue.WorkItem, error) {
	if f.err != nil {
		return queue.WorkItem{}, f.err
	}
	f.items = append(f.items, inputs)
	f.priorities = append(f.priorities, priority)
	return queue.WorkItem{ID: int64(len(f.items)), Inputs: inputs, Priority: priority}, nil
}

func (f *fakeQueue) Cleanup(_ context.Context, inputs models.Inputs) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.cleanups = append(f.cleanups, inputs)
	return 1, nil
}

type fakeRealigner struct{ dirs []string }

func (f *fakeRealigner) RealignDir(_ context.Context, dir string) error {
	f.dirs = append(f.dirs, dir)
	return nil
}

// TestLidarrWebhookRenameRealignsOldAndNewDirs verifies the reactive realign
// trigger sweeps both the OLD (previousPath) directory -- where a sidecar strands
// on a move -- and the new directory, each confined to a library root.
func TestLidarrWebhookRenameRealignsOldAndNewDirs(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	oldDir := filepath.Join(root, "OldArtist", "Album")
	newDir := filepath.Join(root, "NewArtist", "Album")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatalf("mkdir old: %v", err)
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatalf("mkdir new: %v", err)
	}
	oldPath := filepath.Join(oldDir, "01. track.flac") // moved away; dir still holds the sidecar
	newPath := filepath.Join(newDir, "01. track.flac")
	if err := os.WriteFile(newPath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write new: %v", err)
	}

	a := &fakeAuth{}
	q := &fakeQueue{}
	fr := &fakeRealigner{}
	h := NewHandler(a, q, "lyrics", WithAllowedRoots([]string{root}), WithRealigner(fr))
	body := `{"eventType":"Rename","artist":{"artistName":"Artist"},"album":{"title":"Album"},` +
		`"renamedTrackFiles":[{"previousPath":"` + oldPath + `","path":"` + newPath + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.bgRealign.Wait() // realign runs in a detached goroutine; wait for it before asserting

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body %q", rec.Code, rec.Body.String())
	}
	got := map[string]bool{}
	for _, d := range fr.dirs {
		got[d] = true
	}
	if !got[oldDir] {
		t.Errorf("realign not run on old dir %q; got dirs %v", oldDir, fr.dirs)
	}
	if !got[newDir] {
		t.Errorf("realign not run on new dir %q; got dirs %v", newDir, fr.dirs)
	}
}

// TestLidarrWebhookDownloadRealignsTrackAndDeletedDirs verifies an import
// (Download) event realigns the new trackFile directory and the directory of a
// replaced/deleted file (the manual-import abandonment case).
func TestLidarrWebhookDownloadRealignsTrackAndDeletedDirs(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	newDir := filepath.Join(root, "New", "Album")
	delDir := filepath.Join(root, "Old", "Album")
	for _, d := range []string{newDir, delDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	newPath := filepath.Join(newDir, "01. track.flac")
	if err := os.WriteFile(newPath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	delPath := filepath.Join(delDir, "01. old.flac") // deleted; dir still holds the sidecar

	a := &fakeAuth{}
	q := &fakeQueue{}
	fr := &fakeRealigner{}
	h := NewHandler(a, q, "lyrics", WithAllowedRoots([]string{root}), WithRealigner(fr), WithInventory(nil))
	body := `{"eventType":"Download","artist":{"artistName":"A"},"album":{"title":"Album"},` +
		`"tracks":[{"title":"track"}],"trackFiles":[{"path":"` + newPath + `"}],` +
		`"deletedFiles":[{"path":"` + delPath + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.bgRealign.Wait() // realign runs in a detached goroutine; wait for it before asserting

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body %q", rec.Code, rec.Body.String())
	}
	got := map[string]bool{}
	for _, d := range fr.dirs {
		got[d] = true
	}
	if !got[newDir] || !got[delDir] {
		t.Errorf("realign dirs = %v; want both new %q and deleted %q", fr.dirs, newDir, delDir)
	}
}

func TestLidarrWebhookDownloadEnqueuesBeforeOK(t *testing.T) {
	a := &fakeAuth{}
	q := &fakeQueue{}
	h := NewHandler(a, q, "lyrics")
	body := `{
		"eventType":"Download",
		"artist":{"artistName":"Artist"},
		"album":{"title":"Album"},
		"tracks":[{"title":"One"},{"title":"Two"}],
		"extra":"ignored"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=query-key", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d; body %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if a.raw != "query-key" || a.required != auth.ScopeWebhook {
		t.Fatalf("auth raw/scope = %q/%q; want query-key/%q", a.raw, a.required, auth.ScopeWebhook)
	}
	if len(q.items) != 2 {
		t.Fatalf("queued items = %d; want 2", len(q.items))
	}
	if q.items[0].Track.ArtistName != "Artist" || q.items[0].Track.TrackName != "One" || q.items[0].Track.AlbumName != "Album" {
		t.Fatalf("first queued item = %+v; want Artist/One/Album", q.items[0].Track)
	}
	if q.items[0].Outdir != "lyrics" || len(q.items[0].OutputPaths) != 1 || q.items[0].OutputPaths[0].Outdir != "lyrics" {
		t.Fatalf("output destination = %+v; want lyrics outdir", q.items[0])
	}
	for i, v := range q.priorities {
		if v != queue.PriorityWebhook {
			t.Fatalf("queued priority[%d] = %d; want %d", i, v, queue.PriorityWebhook)
		}
	}
}

func TestLidarrWebhookBearerAuthAndSingleTrackRetag(t *testing.T) {
	a := &fakeAuth{}
	q := &fakeQueue{}
	h := NewHandler(a, q, "lyrics")
	body := `{"eventType":"TrackRetag","artist":{"artistName":"Artist"},"track":{"title":"One"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer bearer-key")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d; body %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if a.raw != "bearer-key" {
		t.Fatalf("auth raw = %q; want bearer-key", a.raw)
	}
	if len(q.items) != 1 || q.items[0].Track.TrackName != "One" {
		t.Fatalf("queued items = %+v; want one TrackRetag item", q.items)
	}
}

func TestLidarrWebhookLowercaseBearerAuth(t *testing.T) {
	a := &fakeAuth{}
	q := &fakeQueue{}
	h := NewHandler(a, q, "lyrics")
	body := `{"eventType":"TrackRetag","artist":{"artistName":"Artist"},"track":{"title":"One"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr", strings.NewReader(body))
	req.Header.Set("Authorization", "bearer lower-key")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d; body %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if a.raw != "lower-key" {
		t.Fatalf("auth raw = %q; want lower-key", a.raw)
	}
	if len(q.items) != 1 || q.items[0].Track.TrackName != "One" {
		t.Fatalf("queued items = %+v; want one TrackRetag item", q.items)
	}
}

func TestLidarrWebhookLogOnlyEventsDoNotEnqueue(t *testing.T) {
	for _, event := range []string{"Grab", "Rename"} {
		t.Run(event, func(t *testing.T) {
			a := &fakeAuth{}
			q := &fakeQueue{}
			h := NewHandler(a, q, "lyrics")
			req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(`{"eventType":"`+event+`"}`))
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want %d; body %q", rec.Code, http.StatusOK, rec.Body.String())
			}
			if len(q.items) != 0 {
				t.Fatalf("queued items = %+v; want none", q.items)
			}
		})
	}
}

func TestLidarrWebhookDeleteCleansQueuedWork(t *testing.T) {
	a := &fakeAuth{}
	q := &fakeQueue{}
	h := NewHandler(a, q, "lyrics")
	body := `{"eventType":"Delete","artist":{"artistName":"Artist"},"tracks":[{"title":"One"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d; body %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(q.items) != 0 {
		t.Fatalf("queued items = %+v; want none", q.items)
	}
	if len(q.cleanups) != 1 || q.cleanups[0].Track.ArtistName != "Artist" || q.cleanups[0].Track.TrackName != "One" {
		t.Fatalf("cleanups = %+v; want Artist/One cleanup", q.cleanups)
	}
}

func TestLidarrWebhookAuthAndEnqueueErrors(t *testing.T) {
	t.Run("unauthorized", func(t *testing.T) {
		h := NewHandler(&fakeAuth{err: auth.ErrInvalidKey}, &fakeQueue{}, "lyrics")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=bad", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d; want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("forbidden", func(t *testing.T) {
		h := NewHandler(&fakeAuth{err: auth.ErrForbiddenScope}, &fakeQueue{}, "lyrics")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=bad", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d; want %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("auth backend failure is retryable", func(t *testing.T) {
		h := NewHandler(&fakeAuth{err: errors.New("auth store down")}, &fakeQueue{}, "lyrics")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	t.Run("enqueue failure is retryable", func(t *testing.T) {
		h := NewHandler(&fakeAuth{}, &fakeQueue{err: errors.New("db down")}, "lyrics")
		body := `{"eventType":"Download","artist":{"artistName":"Artist"},"tracks":[{"title":"One"}]}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(body))
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; want %d", rec.Code, http.StatusInternalServerError)
		}
	})
}

type fakeInventory struct {
	results []models.ScanResult
	err     error
	artist  string
	title   string
}

func (f *fakeInventory) FindByTrack(_ context.Context, artist, title string) ([]models.ScanResult, error) {
	f.artist = artist
	f.title = title
	return f.results, f.err
}

const downloadBody = `{"eventType":"Download","artist":{"artistName":"Artist"},"album":{"title":"Album"},"tracks":[{"title":"Song"}]}`

func postWebhook(t *testing.T, h *Handler, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status = %d; want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

// postDownloadWithPaths posts a Download webhook with the given track titles and
// trackFiles paths, JSON-encoding the body so real filesystem paths (which may
// contain characters needing escaping) are transmitted safely.
func postDownloadWithPaths(t *testing.T, h *Handler, titles, paths []string) {
	t.Helper()
	tracks := make([]map[string]string, len(titles))
	for i, title := range titles {
		tracks[i] = map[string]string{"title": title}
	}
	trackFiles := make([]map[string]string, len(paths))
	for i, p := range paths {
		trackFiles[i] = map[string]string{"path": p}
	}
	body, err := json.Marshal(map[string]any{
		"eventType":  "Download",
		"artist":     map[string]string{"artistName": "Artist"},
		"album":      map[string]string{"title": "Album"},
		"tracks":     tracks,
		"trackFiles": trackFiles,
	})
	if err != nil {
		t.Fatalf("marshal webhook body: %v", err)
	}
	postWebhook(t, h, string(body))
}

func TestLidarrWebhookResolvesThroughInventory(t *testing.T) {
	q := &fakeQueue{}
	inv := &fakeInventory{results: []models.ScanResult{{
		ID:       7,
		FilePath: "/music/Artist/Album/song.flac",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Song"},
		Outdir:   "/music/Artist/Album",
		Filename: "song.lrc",
		Status:   "pending",
	}}}
	h := NewHandler(&fakeAuth{}, q, "lyrics", WithInventory(inv))
	postWebhook(t, h, downloadBody)

	if inv.artist != "Artist" || inv.title != "Song" {
		t.Fatalf("FindByTrack args = %q/%q; want Artist/Song", inv.artist, inv.title)
	}
	if len(q.items) != 1 {
		t.Fatalf("enqueued %d items; want 1", len(q.items))
	}
	got := q.items[0]
	if got.SourcePath != "/music/Artist/Album/song.flac" {
		t.Errorf("SourcePath = %q; want the inventory file path", got.SourcePath)
	}
	if got.Outdir != "/music/Artist/Album" || got.Filename != "song.lrc" {
		t.Errorf("output = %q/%q; want inventory outdir/filename", got.Outdir, got.Filename)
	}
	if got.ScanResultID != 7 {
		t.Errorf("ScanResultID = %d; want 7 (linked to scan result)", got.ScanResultID)
	}
	if len(got.OutputPaths) != 1 || got.OutputPaths[0].Outdir != "/music/Artist/Album" {
		t.Errorf("OutputPaths = %+v; want single inventory destination", got.OutputPaths)
	}
}

func TestLidarrWebhookUsesDirectPayloadPath(t *testing.T) {
	root := t.TempDir()
	artistDir := filepath.Join(root, "Artist")
	if err := os.MkdirAll(artistDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file := filepath.Join(artistDir, "song.flac")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	q := &fakeQueue{}
	h := NewHandler(&fakeAuth{}, q, "lyrics",
		WithInventory(&fakeInventory{}),
		WithAllowedRoots([]string{root}),
	)
	postDownloadWithPaths(t, h, []string{"Song"}, []string{file})

	if len(q.items) != 1 {
		t.Fatalf("enqueued %d items; want 1", len(q.items))
	}
	got := q.items[0]
	// The handler must carry the resolved (symlink-free) path forward so the
	// checked path and the written path are identical.
	if got.SourcePath != resolved {
		t.Errorf("SourcePath = %q; want the resolved payload path %q", got.SourcePath, resolved)
	}
	if got.Outdir != filepath.Dir(resolved) || got.Filename != "song.lrc" {
		t.Errorf("output = %q/%q; want resolved path-derived destination", got.Outdir, got.Filename)
	}
}

func TestLidarrWebhookFallsBackWhenPayloadPathUnusable(t *testing.T) {
	// The path is within a configured root but does not exist, so resolution
	// fails and the handler falls back to metadata rather than targeting it.
	root := t.TempDir()
	missing := filepath.Join(root, "Artist", "song.flac")

	q := &fakeQueue{}
	h := NewHandler(&fakeAuth{}, q, "lyrics",
		WithInventory(&fakeInventory{}),
		WithAllowedRoots([]string{root}),
	)
	postDownloadWithPaths(t, h, []string{"Song"}, []string{missing})

	if len(q.items) != 1 {
		t.Fatalf("enqueued %d items; want 1", len(q.items))
	}
	got := q.items[0]
	if got.SourcePath != "" {
		t.Errorf("SourcePath = %q; want empty (unusable path must not be used)", got.SourcePath)
	}
	if got.Outdir != "lyrics" {
		t.Errorf("Outdir = %q; want metadata fallback to configured outdir", got.Outdir)
	}
}

func TestLidarrWebhookRejectsSymlinkEscape(t *testing.T) {
	// A symlink that lives inside a configured root but points outside it passes
	// the lexical check yet must be rejected once symlinks are resolved, so the
	// .lrc write cannot be steered out of the root. (Fails before the symlink
	// hardening, passes after.)
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.flac")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(root, "song.flac")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	q := &fakeQueue{}
	h := NewHandler(&fakeAuth{}, q, "lyrics",
		WithInventory(&fakeInventory{}),
		WithAllowedRoots([]string{root}),
	)
	postDownloadWithPaths(t, h, []string{"Song"}, []string{link})

	if len(q.items) != 1 {
		t.Fatalf("enqueued %d items; want 1", len(q.items))
	}
	got := q.items[0]
	if got.SourcePath != "" {
		t.Errorf("SourcePath = %q; want empty (symlink escaping the root must be rejected)", got.SourcePath)
	}
	if got.Outdir != "lyrics" {
		t.Errorf("Outdir = %q; want metadata fallback, not the symlink target's directory", got.Outdir)
	}
}

func TestLidarrWebhookMultiTrackPayloadPathConfinement(t *testing.T) {
	// Two tracks: one payload path inside the root (usable) and one outside it
	// (rejected). Confinement must be applied per path within the multi-track
	// loop, not just to the first.
	root := t.TempDir()
	inRoot := filepath.Join(root, "SongOne.flac")
	if err := os.WriteFile(inRoot, []byte("x"), 0o644); err != nil {
		t.Fatalf("write in-root file: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(inRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "SongTwo.flac")

	q := &fakeQueue{}
	h := NewHandler(&fakeAuth{}, q, "lyrics",
		WithInventory(&fakeInventory{}),
		WithAllowedRoots([]string{root}),
	)
	postDownloadWithPaths(t, h, []string{"SongOne", "SongTwo"}, []string{inRoot, outside})

	if len(q.items) != 2 {
		t.Fatalf("enqueued %d items; want 2", len(q.items))
	}
	if q.items[0].SourcePath != resolved {
		t.Errorf("SongOne SourcePath = %q; want the resolved in-root path %q", q.items[0].SourcePath, resolved)
	}
	if q.items[1].SourcePath != "" || q.items[1].Outdir != "lyrics" {
		t.Errorf("SongTwo = %q/%q; want metadata fallback (path outside root)", q.items[1].SourcePath, q.items[1].Outdir)
	}
}

func TestPickByAlbumDisambiguatesByAlbum(t *testing.T) {
	results := []models.ScanResult{
		{ID: 1, FilePath: "/music/Artist/Greatest Hits/song.flac"},
		{ID: 2, FilePath: "/music/Artist/Live Album/song.flac"},
	}
	got, ok := pickByAlbum(results, "Live Album")
	if !ok || got.ID != 2 {
		t.Errorf("pickByAlbum(Live Album) = %+v, ok=%v; want ID 2", got, ok)
	}
	// No album hint falls back to the first result (FindByTrack's ordering).
	got, ok = pickByAlbum(results, "")
	if !ok || got.ID != 1 {
		t.Errorf("pickByAlbum(empty) = %+v, ok=%v; want first result ID 1", got, ok)
	}
	// An album hint that matches nothing also falls back to the first result.
	got, ok = pickByAlbum(results, "Nonexistent Album")
	if !ok || got.ID != 1 {
		t.Errorf("pickByAlbum(no match) = %+v, ok=%v; want first result ID 1", got, ok)
	}
	if _, ok := pickByAlbum(nil, "x"); ok {
		t.Error("pickByAlbum(nil) ok = true; want false")
	}
}

func TestLidarrWebhookRejectsPayloadPathOutsideRoots(t *testing.T) {
	// A payload path that "exists" (checker always succeeds) but lies outside
	// every configured library root must NOT be used as a write target; it falls
	// back to metadata. This is the path-injection guard for alert #14.
	q := &fakeQueue{}
	h := NewHandler(&fakeAuth{}, q, "lyrics",
		WithInventory(&fakeInventory{}),
		WithAllowedRoots([]string{"/media/music"}),
		WithPathChecker(func(string) error { return nil }), // pretend everything exists
	)
	body := `{"eventType":"Download","artist":{"artistName":"Artist"},"tracks":[{"title":"Song"}],"trackFiles":[{"path":"/etc/cron.d/evil"}]}`
	postWebhook(t, h, body)

	if len(q.items) != 1 {
		t.Fatalf("enqueued %d items; want 1", len(q.items))
	}
	got := q.items[0]
	if got.SourcePath != "" {
		t.Errorf("SourcePath = %q; want empty (path outside library roots must be rejected)", got.SourcePath)
	}
	if got.Outdir != "lyrics" {
		t.Errorf("Outdir = %q; want metadata fallback, not the attacker-chosen directory", got.Outdir)
	}
}

func TestLidarrWebhookRejectsTraversalOutOfRoot(t *testing.T) {
	// A traversal payload path that resolves outside the root after cleaning is
	// rejected even though its textual prefix matches the root.
	q := &fakeQueue{}
	h := NewHandler(&fakeAuth{}, q, "lyrics",
		WithInventory(&fakeInventory{}),
		WithAllowedRoots([]string{"/media/music"}),
		WithPathChecker(func(string) error { return nil }),
	)
	body := `{"eventType":"Download","artist":{"artistName":"Artist"},"tracks":[{"title":"Song"}],"trackFiles":[{"path":"/media/music/../../etc/evil.mp3"}]}`
	postWebhook(t, h, body)

	got := q.items[0]
	if got.SourcePath != "" || got.Outdir != "lyrics" {
		t.Errorf("got %q/%q; want metadata fallback (traversal escaped the root)", got.SourcePath, got.Outdir)
	}
}

func TestLidarrWebhookPayloadPathDisabledWithoutRoots(t *testing.T) {
	// With no configured roots there is no trusted base to confine against, so
	// raw payload paths are never used regardless of whether they "exist".
	q := &fakeQueue{}
	h := NewHandler(&fakeAuth{}, q, "lyrics",
		WithInventory(&fakeInventory{}),
		WithPathChecker(func(string) error { return nil }),
	)
	body := `{"eventType":"Download","artist":{"artistName":"Artist"},"tracks":[{"title":"Song"}],"trackFiles":[{"path":"/media/music/Artist/song.mp3"}]}`
	postWebhook(t, h, body)

	got := q.items[0]
	if got.SourcePath != "" || got.Outdir != "lyrics" {
		t.Errorf("got %q/%q; want metadata fallback when no roots are configured", got.SourcePath, got.Outdir)
	}
}

func TestDefaultPathCheckerRejectsDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := defaultPathChecker(dir); err == nil {
		t.Error("defaultPathChecker(dir) = nil; want error (directories are not valid targets)")
	}
	file := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := defaultPathChecker(file); err != nil {
		t.Errorf("defaultPathChecker(file) = %v; want nil for a regular file", err)
	}
	if err := defaultPathChecker(filepath.Join(dir, "missing.flac")); err == nil {
		t.Error("defaultPathChecker(missing) = nil; want error for a nonexistent path")
	}
}

func TestLidarrWebhookFallsBackToMetadata(t *testing.T) {
	q := &fakeQueue{}
	h := NewHandler(&fakeAuth{}, q, "lyrics", WithInventory(&fakeInventory{}))
	postWebhook(t, h, downloadBody)

	if len(q.items) != 1 {
		t.Fatalf("enqueued %d items; want 1", len(q.items))
	}
	got := q.items[0]
	if got.SourcePath != "" || got.Outdir != "lyrics" {
		t.Errorf("got %q/%q; want empty source and configured outdir fallback", got.SourcePath, got.Outdir)
	}
	if got.Track.AlbumName != "Album" {
		t.Errorf("AlbumName = %q; want Album carried through metadata fallback", got.Track.AlbumName)
	}
}

func TestLidarrWebhookInventoryErrorDoesNotHardFail(t *testing.T) {
	q := &fakeQueue{}
	h := NewHandler(&fakeAuth{}, q, "lyrics", WithInventory(&fakeInventory{err: errors.New("inventory db down")}))
	postWebhook(t, h, downloadBody)

	if len(q.items) != 1 {
		t.Fatalf("enqueued %d items; want 1 (inventory error must fall back, not fail)", len(q.items))
	}
	if q.items[0].Outdir != "lyrics" {
		t.Errorf("Outdir = %q; want metadata fallback after inventory error", q.items[0].Outdir)
	}
}

type fakeReadiness struct{ err error }

func (f *fakeReadiness) PingContext(_ context.Context) error { return f.err }

type fakeStats struct {
	counts map[string]int64
	err    error
}

func (f *fakeStats) CountByStatus(_ context.Context) (map[string]int64, error) {
	return f.counts, f.err
}

func TestHealthzReturnsOK(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("healthz body = %q; want status ok", rec.Body.String())
	}
}

func TestReadyzReportsDatabase(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", WithReadiness(&fakeReadiness{}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("readyz status = %d; want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"database":"ok"`) {
			t.Fatalf("readyz body = %q; want database ok", rec.Body.String())
		}
	})

	t.Run("no checker omits database claim", func(t *testing.T) {
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics") // no WithReadiness
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("readyz status = %d; want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"status":"ready"`) {
			t.Fatalf("readyz body = %q; want status ready", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "database") {
			t.Fatalf("readyz body = %q; must not claim a database check when none is configured", rec.Body.String())
		}
	})

	t.Run("database unavailable", func(t *testing.T) {
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", WithReadiness(&fakeReadiness{err: errors.New("db file /secret/path down")}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz status = %d; want 503", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "secret") {
			t.Fatalf("readyz body = %q; leaked error detail", rec.Body.String())
		}
	})
}

func TestStatusRequiresAdminAndReportsQueue(t *testing.T) {
	a := &fakeAuth{}
	stats := &fakeStats{counts: map[string]int64{"pending": 3, "failed": 1}}
	h := NewHandler(a, &fakeQueue{}, "lyrics", WithReadiness(&fakeReadiness{}), WithStatusReporter(stats))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status?apikey=secret", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d; want 200", rec.Code)
	}
	if a.required != auth.ScopeAdmin {
		t.Fatalf("status required scope = %q; want admin", a.required)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"pending":3`) || !strings.Contains(body, `"failed":1`) {
		t.Fatalf("status body = %q; want queue counts", body)
	}
}

func TestStatusForbiddenWithoutAdminScope(t *testing.T) {
	h := NewHandler(&fakeAuth{err: auth.ErrForbiddenScope}, &fakeQueue{}, "lyrics", WithStatusReporter(&fakeStats{}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status?apikey=key", nil))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status code = %d; want 403", rec.Code)
	}
}

func TestRedactURIHidesAPIKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=secret&x=1", nil)
	got := redactURI(req.URL)
	if strings.Contains(got, "secret") {
		t.Fatalf("redacted URI = %q; contains secret", got)
	}
	if !strings.Contains(got, "apikey=REDACTED") {
		t.Fatalf("redacted URI = %q; want redacted apikey", got)
	}
}
