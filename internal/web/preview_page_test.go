package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/trustnet"
)

func (f *previewFixture) page(id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/preview/"+id, nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *previewFixture) put(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const (
	pageLRC  = "[00:01.00]Hello there\n[00:05.50]Second line\n"
	pageELRC = "[by:canticle]\n[00:01.00]<00:01.00>Hello <00:01.50>there\n"
)

func TestPreviewPageRendersLinesAndWords(t *testing.T) {
	f := newPreviewFixture(t)
	id := f.row(t, f.writeFile(t, f.root, "song.flac"))
	f.put(t, "song.lrc", pageLRC)
	f.put(t, "song.elrc", pageELRC)

	rec := f.page(itoa(id))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`src="/preview/` + itoa(id) + `/audio"`,
		`data-start-ms="1000"`,
		`data-start-ms="5500"`,
		`<span class="mx-preview-word" data-start-ms="1000">Hello</span>`,
		`<span class="mx-preview-word" data-start-ms="1500">there</span>`,
		`Second line`,
		`/static/css/preview.css`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// The line without word timing stays a plain cue.
	if strings.Contains(body, `data-start-ms="5500"><span`) {
		t.Error("line without words rendered word spans")
	}
	if strings.Contains(body, ` style="`) || strings.Contains(body, "<style") {
		t.Error("page carries inline style, blocked by CSP")
	}
}

func TestPreviewPageLineOnlyHasNoWordSpans(t *testing.T) {
	f := newPreviewFixture(t)
	id := f.row(t, f.writeFile(t, f.root, "song.flac"))
	f.put(t, "song.lrc", pageLRC)

	rec := f.page(itoa(id))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `class="mx-preview-word"`) {
		t.Error("line-only sidecar rendered word spans")
	}
}

func TestPreviewPageInlineWordsInLRC(t *testing.T) {
	f := newPreviewFixture(t)
	id := f.row(t, f.writeFile(t, f.root, "song.flac"))
	f.put(t, "song.lrc", "[00:02.00]<00:02.00>One <00:02.40>two\n")

	body := f.page(itoa(id)).Body.String()
	if !strings.Contains(body, `<span class="mx-preview-word" data-start-ms="2400">two</span>`) {
		t.Errorf("inline A2 words not rendered: %s", body)
	}
}

func TestPreviewPageIgnoresForeignCompanion(t *testing.T) {
	f := newPreviewFixture(t)
	id := f.row(t, f.writeFile(t, f.root, "song.flac"))
	f.put(t, "song.lrc", pageLRC)
	f.put(t, "song.elrc", strings.Replace(pageELRC, "[by:canticle]", "[by:someone]", 1))

	if strings.Contains(f.page(itoa(id)).Body.String(), `class="mx-preview-word"`) {
		t.Error("foreign .elrc words were rendered")
	}
}

func TestPreviewPageRefusals(t *testing.T) {
	f := newPreviewFixture(t)
	noSidecar := f.row(t, f.writeFile(t, f.root, "bare.flac"))
	outsideAudio := f.writeFile(t, f.outside, "out.flac")
	if err := os.WriteFile(filepath.Join(f.outside, "out.lrc"), []byte(pageLRC), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := f.row(t, outsideAudio)

	cases := map[string]string{
		"no sidecar":     itoa(noSidecar),
		"outside roots":  itoa(outside),
		"unknown id":     "999999",
		"non-numeric id": "abc",
		"zero id":        "0",
		"negative id":    "-4",
	}
	for name, id := range cases {
		rec := f.page(id)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "Hello there") {
			t.Errorf("%s: body leaked lyrics", name)
		}
	}
}

func TestPreviewPageRequiresSession(t *testing.T) {
	a, _ := newTestAuth(t, trustnet.LoopbackOnly())
	mux := http.NewServeMux()
	NewUI(config.Config{}, "v-test", WithAuth(a), WithReports(reports.New(openReportsTestDB(t)))).Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/preview/1", nil)
	req.RemoteAddr = "198.51.100.30:1"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("unauthenticated = %d -> %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
}

func TestPreviewPageUnwiredReportsIs503(t *testing.T) {
	mux := http.NewServeMux()
	NewUI(config.Config{}, "v-test").Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/preview/1", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
