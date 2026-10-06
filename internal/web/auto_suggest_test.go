package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/aligner"
)

// rawPoll GETs the run and returns its body exactly as written.
func (e *autoEnv) rawPoll(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, e.url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("poll = %d %s, want 200", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// suggestRun starts a run of lrc that the aligner answers with res, waits for
// state and returns the poll body as written.
func suggestRun(t *testing.T, lrc string, res aligner.Result, state string) (*autoEnv, string) {
	t.Helper()
	e := newAutoEnv(t, &runFake{res: &res})
	e.put(t, "song.lrc", lrc)
	if rec := e.start(t); rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s, want 202", rec.Code, rec.Body)
	}
	e.waitState(t, state)
	return e, e.rawPoll(t)
}

// The done payload's shape, pinned byte for byte: lines holds one start per
// cue, words per cue its units or null, then quality and warnings.
func TestAutoSuggestionPayload(t *testing.T) {
	e, got := suggestRun(t, "[00:01.00]one two\n[00:05.00]three\n", aligner.Result{
		Transcript: "one two three",
		Words: []aligner.Word{
			{Text: "one", StartMS: 1200, EndMS: 1500, LineIndex: 0, Confidence: 0.5},
			{Text: "two", StartMS: 1600, EndMS: 1900, LineIndex: 0, Confidence: 0.5},
			{Text: "three", StartMS: 5500, EndMS: 5900, LineIndex: 1, Confidence: 0.5},
		},
	}, autoDone)
	want := `{"aligned_words":3,"lines":[1200,5500],"mtime":` + e.mtime(t) +
		`,"quality":{"similarity":1,"has_transcript":true,"mean_confidence":0.5,"coverage":1,"tokens":3,"aligned_tokens":3,"merged":0}` +
		`,"state":"done","warnings":[],"words":[[{"token":0,"start_ms":1200},{"token":1,"start_ms":1600}],null]}` + "\n"
	if got != want {
		t.Errorf("done payload =\n%s\nwant\n%s", got, want)
	}
}

// Warnings arrive in the fixed order similarity, confidence, coverage, merged;
// without a transcript similarity is not warned on, and has_transcript says so.
func TestAutoSuggestionWarningsOrder(t *testing.T) {
	// "two" is never aligned (coverage 2/3), confidence is 0.1, and the two
	// lines land in one hundredth (merged).
	words := []aligner.Word{
		{Text: "one", StartMS: 1200, EndMS: 1250, LineIndex: 0, Confidence: 0.1},
		{Text: "three", StartMS: 1205, EndMS: 1300, LineIndex: 1, Confidence: 0.1},
	}
	for _, tc := range []struct {
		transcript string
		want       []any
		has        bool
	}{
		{"zzz", []any{"similarity", "confidence", "coverage", "merged"}, true},
		{"", []any{"confidence", "coverage", "merged"}, false},
	} {
		e, _ := suggestRun(t, "[00:01.00]one two\n[00:05.00]three\n",
			aligner.Result{Transcript: tc.transcript, Words: words}, autoDone)
		_, body := e.poll()
		q, _ := body["quality"].(map[string]any)
		if !reflect.DeepEqual(body["warnings"], tc.want) || q["has_transcript"] != tc.has || q["merged"] != json.Number("1") {
			t.Errorf("transcript %q: warnings %v quality %v, want warnings %v, has_transcript %v, merged 1",
				tc.transcript, body["warnings"], q, tc.want, tc.has)
		}
	}
}

// A result with no usable word is no_suggestion: its own state, not a failed
// run, and it carries no suggestion fields.
func TestAutoNoSuggestion(t *testing.T) {
	for name, res := range map[string]aligner.Result{
		"no words":           {Transcript: "one"},
		"word names no line": {Words: []aligner.Word{{Text: "one", StartMS: 1000, EndMS: 1100, LineIndex: 7}}},
	} {
		_, got := suggestRun(t, "[00:01.00]one\n", res, autoNoSuggestion)
		want := `{"aligned_words":` + itoa(int64(len(res.Words))) + `,"state":"no_suggestion"}` + "\n"
		if got != want || strings.Contains(got, "error") {
			t.Errorf("%s: payload = %s, want %s", name, got, want)
		}
	}
}
