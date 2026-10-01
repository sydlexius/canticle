package web

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

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

	rec := f.get(strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
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
