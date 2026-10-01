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

func TestPreviewSidecarBoundary(t *testing.T) {
	f := newPreviewFixture(t)
	roots := []string{f.root}
	line := "[00:01.00]aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"
	fill := func(n int) string {
		b := strings.Repeat(line, n/len(line))
		return b + strings.Repeat("x", n-len(b))
	}
	f.put(t, "exact.lrc", fill(previewSidecarMax))
	f.put(t, "over.lrc", fill(previewSidecarMax+1))
	body, cut, ok := readPreviewSidecar(roots, filepath.Join(f.root, "exact.lrc"))
	if !ok || cut || len(body) != previewSidecarMax {
		t.Errorf("exactly max: ok=%v cut=%v len=%d, want ok, not cut, full", ok, cut, len(body))
	}
	body, cut, ok = readPreviewSidecar(roots, filepath.Join(f.root, "over.lrc"))
	if !ok || !cut {
		t.Fatalf("max+1: ok=%v cut=%v, want ok and cut", ok, cut)
	}
	if !strings.HasSuffix(body, "\n") || len(body) > previewSidecarMax {
		t.Errorf("cut body must end on a complete line within the bound, len=%d", len(body))
	}
}

func TestPreviewPageTruncatedLRCShowsNotice(t *testing.T) {
	f := newPreviewFixture(t)
	id := f.row(t, f.writeFile(t, f.root, "song.flac"))
	f.put(t, "song.lrc", strings.Repeat("[00:01.00]line of lyric text here\n", previewSidecarMax/30))
	if !strings.Contains(f.page(itoa(id)).Body.String(), `class="mx-preview-notice"`) {
		t.Error("oversized .lrc rendered without a truncation notice")
	}
	id2 := f.row(t, f.writeFile(t, f.root, "ok.flac"))
	f.put(t, "ok.lrc", pageLRC)
	if strings.Contains(f.page(itoa(id2)).Body.String(), `class="mx-preview-notice"`) {
		t.Error("normal .lrc rendered a truncation notice")
	}
}

func TestPreviewLinesCompanionOwnershipIsHeaderOnly(t *testing.T) {
	foreign := "[00:01.00]<00:01.00>Hello <00:01.50>there\n[by:canticle]\n"
	if _, hasWords := previewLines(pageLRC, foreign); hasWords {
		t.Error("a [by:canticle] tag after a cue made a foreign companion owned")
	}
	if _, hasWords := previewLines(pageLRC, pageELRC); !hasWords {
		t.Error("a header [by:canticle] companion was not honored")
	}
}

func TestPreviewLinesDuplicateStartsMergeByOccurrence(t *testing.T) {
	lrc := "[00:01.00]First one\n[00:01.00]Second two\n"
	elrc := "[by:canticle]\n[00:01.00]<00:01.00>First <00:01.20>one\n[00:01.00]<00:01.00>Second <00:01.30>two\n"
	lines, _ := previewLines(lrc, elrc)
	if len(lines) != 2 || len(lines[0].Words) != 2 || len(lines[1].Words) != 2 {
		t.Fatalf("lines = %+v, want two lines of two words", lines)
	}
	if lines[0].Words[1].Text != "one" || lines[1].Words[1].Text != "two" || lines[1].Words[1].StartMS != "1300" {
		t.Errorf("same-start lines got each other's words: %+v", lines)
	}
}

func TestPreviewLinesWordSeparatorsAreFaithful(t *testing.T) {
	cjk, _ := previewLines("[00:01.00]你好\n", "[by:canticle]\n[00:01.00]<00:01.00>你<00:01.50>好\n")
	if len(cjk[0].Words) != 2 || cjk[0].Words[1].Before != "" {
		t.Errorf("CJK words must carry no separator: %+v", cjk[0].Words)
	}
	lead, _ := previewLines("[00:01.00]La <00:01.20>da <00:01.60>dee\n", "")
	w := lead[0].Words
	if len(w) != 2 || w[0].Before != "La " || w[1].Before != " " || w[1].Text != "dee" {
		t.Errorf("leading unmarked text lost: %+v", w)
	}
	// A companion whose words are not in the line's text cannot be
	// reconstructed: the line stays plain rather than rendering wrong text.
	stale, hasWords := previewLines("[00:01.00]Hello there\n", "[by:canticle]\n[00:01.00]<00:01.00>Other <00:01.50>words\n")
	if hasWords || len(stale[0].Words) != 0 {
		t.Errorf("mismatched companion words attached: %+v", stale[0])
	}
}

func TestPreviewPageRendersCJKWithoutSpaces(t *testing.T) {
	f := newPreviewFixture(t)
	id := f.row(t, f.writeFile(t, f.root, "song.flac"))
	f.put(t, "song.lrc", "[00:01.00]你好\n")
	f.put(t, "song.elrc", "[by:canticle]\n[00:01.00]<00:01.00>你<00:01.50>好\n")
	body := f.page(itoa(id)).Body.String()
	if !strings.Contains(body, `data-start-ms="1000">你</span><span class="mx-preview-word" data-start-ms="1500">好</span>`) {
		t.Errorf("CJK words not adjacent: %s", body)
	}
}
