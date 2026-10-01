package web

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/trustnet"
)

var (
	previewBytes = []byte("0123456789abcdefghij")
	rowSeq       atomic.Int64
)

// previewFixture is one library root plus a sibling directory outside it.
type previewFixture struct {
	root, outside string
	db            *sql.DB
	mux           *http.ServeMux
}

func newPreviewFixture(t *testing.T) *previewFixture {
	t.Helper()
	f := &previewFixture{root: t.TempDir(), outside: t.TempDir(), db: openReportsTestDB(t)}
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO libraries (path, name) VALUES (?, 'lib')`, f.root); err != nil {
		t.Fatalf("seed library: %v", err)
	}
	f.mux = newReportsUIServer(t, f.db)
	return f
}

func (f *previewFixture) writeFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, previewBytes, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func (f *previewFixture) row(t *testing.T, sourcePath string) int64 {
	t.Helper()
	var id int64
	if err := f.db.QueryRowContext(context.Background(),
		`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, source_path)
		 VALUES ('A', 'T', 'a', ?, 'Al', 'done', ?) RETURNING id`,
		"t"+strconv.Itoa(int(rowSeq.Add(1))), sourcePath).Scan(&id); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	return id
}

func (f *previewFixture) get(id string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/preview/"+id+"/audio", nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func TestPreviewAudioServesBytesWithHeaders(t *testing.T) {
	f := newPreviewFixture(t)
	id := f.row(t, f.writeFile(t, f.root, "song.flac"))

	// httptest.ResponseRecorder supports no write deadline, so this request
	// also exercises the deadline-failure branch, which must log loudly.
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := f.get(strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := logs.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "cannot extend the write deadline") {
		t.Errorf("deadline failure not logged at ERROR: %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), previewBytes) {
		t.Errorf("body = %q, want %q", rec.Body.Bytes(), previewBytes)
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/flac" {
		t.Errorf("Content-Type = %q, want audio/flac", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Errorf("Cross-Origin-Resource-Policy = %q, want same-origin", got)
	}
}

func TestPreviewAudioRange(t *testing.T) {
	f := newPreviewFixture(t)
	id := strconv.FormatInt(f.row(t, f.writeFile(t, f.root, "song.mp3")), 10)

	rec := f.get(id, "Range", "bytes=5-9")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != "56789" {
		t.Errorf("body = %q, want 56789", got)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 5-9/20" {
		t.Errorf("Content-Range = %q, want bytes 5-9/20", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestPreviewAudioRefusals(t *testing.T) {
	f := newPreviewFixture(t)
	secret := f.writeFile(t, f.outside, "secret.flac")
	link := filepath.Join(f.root, "link.flac")
	symlinkOK := os.Symlink(secret, link) == nil
	dirLink := filepath.Join(f.root, "dirlink")
	dirLinkOK := os.Symlink(f.outside, dirLink) == nil

	cases := map[string]string{
		"missing file":          filepath.Join(f.root, "gone.flac"),
		"outside every root":    secret,
		"dotdot out of root":    filepath.Join(f.root, "..", filepath.Base(f.outside), "secret.flac"),
		"relative path":         "song.flac",
		"empty path":            "",
		"directory, not a file": f.root,
	}
	if symlinkOK {
		cases["symlink escaping root"] = link
	}
	if dirLinkOK {
		cases["symlinked dir escaping root"] = filepath.Join(dirLink, "secret.flac")
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.get(strconv.FormatInt(f.row(t, path), 10))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if path != "" && strings.Contains(rec.Body.String(), path) {
				t.Errorf("404 body leaks the path: %q", rec.Body.String())
			}
			if bytes.Contains(rec.Body.Bytes(), previewBytes) {
				t.Error("404 body carries file content")
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
				t.Errorf("Cross-Origin-Resource-Policy = %q, want same-origin", got)
			}
		})
	}
}

func TestPreviewAudioUnknownOrBadID(t *testing.T) {
	f := newPreviewFixture(t)
	for _, id := range []string{"99999", "0", "-3", "abc", "1.5"} {
		if rec := f.get(id); rec.Code != http.StatusNotFound {
			t.Errorf("id %q: status = %d, want 404", id, rec.Code)
		}
	}
}

func TestPreviewAudioRootRemovedWhileServing(t *testing.T) {
	f := newPreviewFixture(t)
	id := strconv.FormatInt(f.row(t, f.writeFile(t, f.root, "song.flac")), 10)
	if rec := f.get(id); rec.Code != http.StatusOK {
		t.Fatalf("before removal: status = %d, want 200", rec.Code)
	}
	if _, err := f.db.ExecContext(context.Background(), `DELETE FROM libraries`); err != nil {
		t.Fatalf("delete libraries: %v", err)
	}
	if rec := f.get(id); rec.Code != http.StatusNotFound {
		t.Fatalf("after removal: status = %d, want 404 (roots are read live)", rec.Code)
	}
}

func TestPreviewAudioWithoutReportsRepo(t *testing.T) {
	mux := http.NewServeMux()
	NewUI(config.Config{}, "v-test").Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/preview/1/audio", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestPreviewAudioRequiresSession(t *testing.T) {
	a, svc := newTestAuth(t, trustnet.LoopbackOnly())
	f := newPreviewFixture(t)
	mux := http.NewServeMux()
	NewUI(config.Config{}, "vtest", WithAuth(a), WithReports(reports.New(f.db))).Register(mux)
	id := strconv.FormatInt(f.row(t, f.writeFile(t, f.root, "song.flac")), 10)

	req := httptest.NewRequest(http.MethodGet, "/preview/"+id+"/audio", nil)
	req.RemoteAddr = "198.51.100.31:1"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Fatalf("no session: %d -> %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if bytes.Contains(rec.Body.Bytes(), previewBytes) {
		t.Error("unauthenticated response carries file content")
	}

	req = httptest.NewRequest(http.MethodGet, "/preview/"+id+"/audio", nil)
	req.RemoteAddr = "198.51.100.31:1"
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: loginToken(t, svc)})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), previewBytes) {
		t.Fatalf("with session: status = %d, want 200 with the bytes", rec.Code)
	}
}

func TestPreviewAudioNeverLogsThePath(t *testing.T) {
	f := newPreviewFixture(t)
	secret := f.writeFile(t, f.outside, "very-distinctive-name.flac")
	gone := filepath.Join(f.root, "also-distinctive-missing.flac")
	good := f.writeFile(t, f.root, "yet-another-distinctive.flac")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, p := range []string{secret, gone, good} {
		f.get(strconv.FormatInt(f.row(t, p), 10))
	}
	log := buf.String()
	if !strings.Contains(log, "preview audio refused") {
		t.Fatalf("expected a refusal log line, got %q", log)
	}
	for _, p := range []string{secret, gone, good, "distinctive", f.root, f.outside} {
		if strings.Contains(log, p) {
			t.Errorf("log leaks path fragment %q: %s", p, log)
		}
	}
}

func TestPreviewContentType(t *testing.T) {
	for path, want := range map[string]string{
		"a.MP3":  "audio/mpeg",
		"a.m4a":  "audio/mp4",
		"a.opus": "audio/ogg",
		"a.wav":  "audio/wav",
		"a.xyz":  "application/octet-stream",
		"a":      "application/octet-stream",
	} {
		if got := previewContentType(path); got != want {
			t.Errorf("previewContentType(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestPreviewAudioIntermediateSwapEscapes pins F1 of the #481 review: root/Album
// was a real directory when the row was written and is a symlink to an outside
// directory at open time. Confinement happens AT the open (os.Root), not in an
// earlier check, so this post-swap state is exactly what the open sees; it must
// 404 and never serve the outside file.
func TestPreviewAudioIntermediateSwapEscapes(t *testing.T) {
	f := newPreviewFixture(t)
	secret := []byte("SECRET-OUTSIDE-CONTENT")
	if err := os.WriteFile(filepath.Join(f.outside, "song.flac"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	album := filepath.Join(f.root, "Album")
	if err := os.Mkdir(album, 0o755); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(f.row(t, f.writeFile(t, album, "song.flac")), 10)
	if rec := f.get(id); rec.Code != http.StatusOK {
		t.Fatalf("before swap: status = %d, want 200", rec.Code)
	}
	if err := os.RemoveAll(album); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.outside, album); err != nil {
		t.Skipf("symlink: %v", err)
	}
	rec := f.get(id)
	if bytes.Contains(rec.Body.Bytes(), secret) {
		t.Fatal("served an out-of-root file through a swapped intermediate directory")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("after swap: status = %d, want 404", rec.Code)
	}
}

// TestPreviewAudioSymlinkedRoot pins F3: a root configured through a symlink
// serves a row stored in either spelling, since the Lidarr webhook stores the
// resolved path while the scanner stores the configured one.
func TestPreviewAudioSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "music")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	f := &previewFixture{root: link, outside: t.TempDir(), db: openReportsTestDB(t)}
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO libraries (path, name) VALUES (?, 'lib')`, link); err != nil {
		t.Fatal(err)
	}
	f.mux = newReportsUIServer(t, f.db)
	resolved, err := filepath.EvalSymlinks(f.writeFile(t, real, "song.flac"))
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{
		"configured spelling": filepath.Join(link, "song.flac"),
		"resolved spelling":   resolved,
	} {
		rec := f.get(strconv.FormatInt(f.row(t, src), 10))
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), previewBytes) {
			t.Errorf("%s: status = %d, want 200 with the bytes", name, rec.Code)
		}
	}
}

// TestPreviewAudioOutlivesServerWriteTimeout pins F2: the server-wide
// WriteTimeout (15s in production, scaled down here) must not cut a slowly
// consumed audio stream, which a buffering <audio> element always is.
func TestPreviewAudioOutlivesServerWriteTimeout(t *testing.T) {
	f := newPreviewFixture(t)
	big := bytes.Repeat([]byte{0x5a}, 8<<20)
	p := filepath.Join(f.root, "big.flac")
	if err := os.WriteFile(p, big, 0o600); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(f.row(t, p), 10)
	srv := httptest.NewUnstartedServer(f.mux)
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/preview/"+id+"/audio", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 512<<10)
	var n int
	start := time.Now()
	for {
		k, rerr := resp.Body.Read(buf)
		n += k
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				t.Fatalf("stream cut after %d of %d bytes in %v: %v", n, len(big), time.Since(start), rerr)
			}
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if n != len(big) {
		t.Fatalf("read %d bytes, want %d", n, len(big))
	}
	if time.Since(start) < srv.Config.WriteTimeout {
		t.Fatalf("stream finished in %v, under the %v WriteTimeout; the test proves nothing", time.Since(start), srv.Config.WriteTimeout)
	}
}

func TestOpenPreviewAudioRootMatching(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "song.flac")
	if err := os.WriteFile(p, previewBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		roots []string
		path  string
		ok    bool
	}{
		"blank root skipped": {[]string{"", root}, p, true},
		"no roots":           {nil, p, false},
		"root is a file":     {[]string{p}, p, false},
		"root itself":        {[]string{root}, root, false},
	} {
		f, _, ok := openPreviewAudio(tc.roots, tc.path)
		if f != nil {
			_ = f.Close()
		}
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", name, ok, tc.ok)
		}
	}
}

// TestRelUnder tests the containment guard directly: behind os.Root an
// end-to-end refusal cannot tell whether this guard or the open refused.
func TestRelUnder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "lib")
	for name, tc := range map[string]struct {
		path, rel string
		ok        bool
	}{
		"beneath":             {filepath.Join(root, "a", "b.flac"), filepath.Join("a", "b.flac"), true},
		"sibling with prefix": {root + "x" + string(filepath.Separator) + "b.flac", "", false},
		"parent":              {filepath.Dir(root), "", false},
		"relative path":       {"b.flac", "", false},
	} {
		rel, ok := relUnder(root, tc.path)
		if ok != tc.ok || rel != tc.rel {
			t.Errorf("%s: relUnder = (%q, %v), want (%q, %v)", name, rel, ok, tc.rel, tc.ok)
		}
	}
}

// TestPreviewAudioAbsoluteSymlinkInsideRoot pins that an ABSOLUTE symlink
// whose target stays inside the root serves, final or intermediate: os.Root
// refuses every absolute symlink, and the scanner enqueues such files. The
// escaping case is "symlink escaping root" in TestPreviewAudioRefusals.
func TestPreviewAudioAbsoluteSymlinkInsideRoot(t *testing.T) {
	f := newPreviewFixture(t)
	b := filepath.Join(f.root, "B")
	if err := os.Mkdir(b, 0o755); err != nil {
		t.Fatal(err)
	}
	target := f.writeFile(t, b, "song.flac")
	final := filepath.Join(f.root, "final.flac")
	album := filepath.Join(f.root, "AlbumLink")
	if os.Symlink(target, final) != nil || os.Symlink(b, album) != nil {
		t.Skip("symlinks unsupported")
	}
	for name, src := range map[string]string{
		"final symlink":        final,
		"intermediate symlink": filepath.Join(album, "song.flac"),
	} {
		rec := f.get(strconv.FormatInt(f.row(t, src), 10))
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), previewBytes) {
			t.Errorf("%s: status = %d, want 200 with the bytes", name, rec.Code)
		}
	}
}
