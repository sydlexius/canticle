package musixmatch

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// EVERYTHING HERE IS SYNTHETIC: placeholder words, round dummy timestamps, no
// identifiers. No provider lyric content and no library metadata may enter these
// fixtures -- a richsync body IS the lyric, and this repository is public.
//
// Fixtures are INLINE rather than under testdata/: the package has no testdata/
// and builds every other response body inline (client_test.go, and slice A's
// commontrack_test.go made the same call), the parser takes raw bytes so a file
// adds no realism, and the #489 test turns on the relationship between five
// entry timestamps and three cue timestamps -- far easier to review with the
// numbers and the assertions on one screen.

// richSyncBody wraps entry JSON the way the wire does -- richsync_body is a JSON
// STRING whose contents are JSON -- so the two-step decode is exercised.
func richSyncBody(t *testing.T, entriesJSON string) []byte {
	t.Helper()
	raw, err := json.Marshal(entriesJSON)
	if err != nil {
		t.Fatalf("encoding the richsync body as a JSON string: %v", err)
	}
	return raw
}

func cue(sec float64, text string) models.Lines {
	return models.Lines{Text: text, Time: models.MsToTime(int(sec * 1000))}
}

// TestParseRichSyncBodyMapsFields covers the field mapping; the FIRST case is
// the highest-value assertion in the file. ts/te/o are believed to be SECONDS AS
// FLOATS and are scaled by 1000; an int decode truncates to whole seconds, every
// chunk shares one stamp, and a2Words' uniformStarts guard then correctly
// refuses the markers -- so the feature does NOTHING rather than failing. Exact
// integers are asserted for that reason: "non-zero" passes under truncation.
func TestParseRichSyncBodyMapsFields(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries string
		cues    []models.Lines
		want    []models.WordTiming
	}{{
		// ts 1.0 + o 0.5 -> 1500 ms. A 1 or a 1000 means the offsets truncated.
		name: "units scale to milliseconds",
		entries: `[{"ts":1.0,"te":3.0,"x":"alpha bravo","l":[
			{"c":"alpha","o":0.5},{"c":" ","o":0.75},{"c":"bravo","o":1.25}]}]`,
		cues: []models.Lines{cue(1.0, "alpha bravo")},
		// Text is VERBATIM, space chunk included: trimming still satisfies
		// a2Words' whitespace-insensitive guard while silently removing the
		// spaces a player renders. EndMS is the next chunk's absolute start, and
		// te for the last one.
		want: []models.WordTiming{
			{Line: 0, Text: "alpha", StartMS: 1500, EndMS: 1750},
			{Line: 0, Text: " ", StartMS: 1750, EndMS: 2250},
			{Line: 0, Text: "bravo", StartMS: 2250, EndMS: 3000},
		},
	}, {
		// Mutually distinguishable on purpose: ts alone gives 4000, o alone 250.
		name:    "the stamp is ts+o, not either alone",
		entries: `[{"ts":4.0,"te":6.0,"x":"alpha","l":[{"c":"alpha","o":0.25}]}]`,
		cues:    []models.Lines{cue(4.0, "alpha")},
		want:    []models.WordTiming{{Line: 0, Text: "alpha", StartMS: 4250, EndMS: 6000}},
	}, {
		// The clamp petitlyrics/decode.go applies, which models.WordTiming states
		// as a requirement on producers rather than a guarantee of the type.
		name: "a negative stamp clamps to zero",
		entries: `[{"ts":0.2,"te":2.0,"x":"alpha bravo","l":[
			{"c":"alpha","o":-0.5},{"c":"bravo","o":1.0}]}]`,
		cues: []models.Lines{cue(0.2, "alpha bravo")},
		want: []models.WordTiming{
			{Line: 0, Text: "alpha", StartMS: 0, EndMS: 1200},
			{Line: 0, Text: "bravo", StartMS: 1200, EndMS: 2000},
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRichSyncBody(richSyncBody(t, tc.entries), tc.cues)
			if err != nil {
				t.Fatalf("parseRichSyncBody: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("emitted %d timings, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("timing %d = %+v, want %+v", i, got[i], w)
				}
			}
		})
	}
}

// TestParseRichSyncBodyNeverReturnsEmptySuccess is the generalized guard,
// modeled on TestParseSubtitleBodyNeverReturnsEmptySuccess: no input may yield a
// nil error from a body carrying no line entries. json.Unmarshal accepts "null"
// without error, which is how parseSubtitleBody's own silent-settle bug arrived.
// The last case is a body ALREADY an array -- one level of encoding missing, an
// unrecognized shape rather than a free pass through the first decode step.
func TestParseRichSyncBodyNeverReturnsEmptySuccess(t *testing.T) {
	cues := []models.Lines{cue(1.0, "alpha")}
	for _, inner := range []string{"null", "[]", "", "  ", "{}", `"nope"`, "0", "false", "[oops"} {
		t.Run(inner, func(t *testing.T) {
			got, err := parseRichSyncBody(richSyncBody(t, inner), cues)
			if err == nil {
				t.Fatalf("parseRichSyncBody(%q) returned SUCCESS with %d timings; "+
					"a body carrying no line entries must be an error", inner, len(got))
			}
			if !errors.Is(err, ErrUnparsableRichSyncBody) {
				t.Errorf("error = %v; want ErrUnparsableRichSyncBody", err)
			}
		})
	}

	raw := []byte(`[{"ts":1.0,"te":2.0,"x":"alpha","l":[{"c":"alpha","o":0.0}]}]`)
	if _, err := parseRichSyncBody(raw, cues); !errors.Is(err, ErrUnparsableRichSyncBody) {
		t.Fatalf("un-stringified body: error = %v; want ErrUnparsableRichSyncBody", err)
	}
}

// TestRichSyncErrorTextCarriesOnlyAByteCount is a privacy/copyright guard, not a
// formatting one: the body is the lyric content and this error lands in
// work_queue.last_error. ErrUnparsableSubtitleBody's "(%d bytes)" is the
// precedent being followed.
func TestRichSyncErrorTextCarriesOnlyAByteCount(t *testing.T) {
	raw := richSyncBody(t, `not json at all: zulu-placeholder-token`)
	_, err := parseRichSyncBody(raw, []models.Lines{cue(1.0, "alpha")})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "zulu-placeholder-token") {
		t.Errorf("error text leaked the body: %q", err.Error())
	}
}

// TestParseRichSyncBodyRejectsAUnitError is the seconds/milliseconds tripwire.
// Without it a 1000x unit error is silent in the worst way: nothing binds,
// nothing is emitted, and the track reads as "no richsync" forever.
func TestParseRichSyncBodyRejectsAUnitError(t *testing.T) {
	cues := []models.Lines{cue(10.0, "alpha"), cue(20.0, "bravo")}
	msBody := richSyncBody(t, `[{"ts":10000,"te":13000,"x":"alpha","l":[{"c":"alpha","o":0}]}]`)
	if _, err := parseRichSyncBody(msBody, cues); !errors.Is(err, ErrUnparsableRichSyncBody) {
		t.Fatalf("error = %v; want ErrUnparsableRichSyncBody. Stamps implying a track orders of "+
			"magnitude longer than its cues must be named, not emitted", err)
	}

	// Control: the SAME body in seconds passes, so the guard discriminates on the
	// unit rather than rejecting everything.
	secBody := richSyncBody(t, `[{"ts":10.0,"te":13.0,"x":"alpha","l":[{"c":"alpha","o":0.0}]}]`)
	if _, err := parseRichSyncBody(secBody, cues); err != nil {
		t.Fatalf("the seconds-valued control was rejected: %v", err)
	}

	// With no cues there is no span to judge against, so the check is skipped
	// rather than guessed at; nothing can bind either way.
	got, err := parseRichSyncBody(msBody, nil)
	if err != nil {
		t.Fatalf("parseRichSyncBody with no cues: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("emitted %d timings against zero cues; nothing can bind", len(got))
	}
}

// TestCorrelationRules covers the binding rule's arms. Refusing on ambiguity is
// deliberate: a false reject costs only one line's markers, since the plain cue
// still writes, while a false bind stamps one line's timing onto another's
// words.
func TestCorrelationRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cues    []models.Lines
		entries string
		// wantLine maps emitted chunk text -> the cue index it must bind to.
		// An empty map means nothing may be emitted.
		wantLine map[string]int
	}{{
		name:     "two candidates and no text match drops the entry",
		cues:     []models.Lines{cue(20.0, "alpha"), cue(20.2, "bravo")},
		entries:  `[{"ts":20.1,"te":21.0,"x":"charlie","l":[{"c":"charlie","o":0.0}]}]`,
		wantLine: map[string]int{},
	}, {
		// Whitespace is ignored (each pipeline joins its own chunks); nothing else.
		name:     "a tie breaks on line text",
		cues:     []models.Lines{cue(20.0, "alpha bravo"), cue(20.2, "charlie delta")},
		entries:  `[{"ts":20.1,"te":21.0,"x":"charliedelta","l":[{"c":"charlie","o":0.0}]}]`,
		wantLine: map[string]int{"charlie": 1},
	}, {
		name:     "an entry outside the tolerance window has no counterpart",
		cues:     []models.Lines{cue(10.0, "alpha")},
		entries:  `[{"ts":10.301,"te":11.0,"x":"alpha","l":[{"c":"alpha","o":0.0}]}]`,
		wantLine: map[string]int{},
	}, {
		name: "first bind wins and a cue is never bound twice",
		cues: []models.Lines{cue(10.0, "alpha")},
		entries: `[{"ts":10.0,"te":11.0,"x":"alpha","l":[{"c":"alpha","o":0.0}]},
			{"ts":10.1,"te":11.0,"x":"bravo","l":[{"c":"bravo","o":0.0}]}]`,
		wantLine: map[string]int{"alpha": 0},
	}, {
		// First-bind-wins is only well defined if the order is the timeline's,
		// not the payload's: the later ts is listed FIRST here.
		name: "entries are processed in ascending ts, not payload order",
		cues: []models.Lines{cue(10.0, "alpha")},
		entries: `[{"ts":10.1,"te":11.0,"x":"bravo","l":[{"c":"bravo","o":0.0}]},
			{"ts":10.0,"te":11.0,"x":"alpha","l":[{"c":"alpha","o":0.0}]}]`,
		wantLine: map[string]int{"alpha": 0},
	}, {
		// The emitted Line is the CALLER's index, so an unsorted cue slice must
		// still bind correctly.
		name:     "cue slice order does not change the emitted index",
		cues:     []models.Lines{cue(30.0, "charlie"), cue(10.0, "alpha"), cue(20.0, "bravo")},
		entries:  `[{"ts":30.0,"te":31.0,"x":"charlie","l":[{"c":"charlie","o":0.0}]}]`,
		wantLine: map[string]int{"charlie": 0},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			before := make([]models.Lines, len(tc.cues))
			copy(before, tc.cues)

			got, err := parseRichSyncBody(richSyncBody(t, tc.entries), tc.cues)
			if err != nil {
				t.Fatalf("parseRichSyncBody: %v", err)
			}
			if len(got) != len(tc.wantLine) {
				t.Fatalf("emitted %d timings, want %d: %+v", len(got), len(tc.wantLine), got)
			}
			for _, wt := range got {
				want, ok := tc.wantLine[wt.Text]
				if !ok {
					t.Errorf("emitted %q, which must not bind at all", wt.Text)
					continue
				}
				if wt.Line != want {
					t.Errorf("%q bound to cue %d, want %d", wt.Text, wt.Line, want)
				}
			}
			// The caller's slice IS song.Subtitles.Lines and its order is the
			// writer's emission order, so it must come back untouched.
			for i := range before {
				if tc.cues[i] != before[i] {
					t.Fatalf("the caller's cue slice was mutated at index %d", i)
				}
			}
		})
	}
}

// --- The #489 regression test -----------------------------------------------

// The fixture diverges on purpose: 5 entries against 3 cues, chosen so exactly
// two entries bind, one is ambiguous, and two have no counterpart. Cue times are
// 10.00 / 20.00 / 20.20; the last two sit 200 ms apart, inside the 300 ms
// window, which is what makes an ambiguous entry constructible at all.
func divergentCues() []models.Lines {
	return []models.Lines{
		cue(10.00, "alpha"),   // index 0
		cue(20.00, "bravo"),   // index 1
		cue(20.20, "charlie"), // index 2
	}
}

const divergentEntriesJSON = `[
	{"ts":2.00, "te":3.00, "x":"delta",   "l":[{"c":"delta","o":0.0}]},
	{"ts":10.00,"te":11.00,"x":"alpha",   "l":[{"c":"alpha","o":0.0}]},
	{"ts":20.10,"te":21.00,"x":"echo",    "l":[{"c":"echo","o":0.0}]},
	{"ts":20.20,"te":21.00,"x":"charlie", "l":[{"c":"charlie","o":0.0}]},
	{"ts":45.00,"te":46.00,"x":"foxtrot", "l":[{"c":"foxtrot","o":0.0}]}
]`

// TestCorrelationDoesNotPairByIndex is the #489 regression test. #489 is an OPEN
// bug here caused by exactly the mistake asserted against: writeSyncedLRC pairs
// a bilingual translation to its original BY SLICE INDEX and silently misaligns
// every line after the counts diverge.
//
// BOTH halves are load-bearing. The first asserts the rule holds. The second is
// the NEGATIVE CONTROL: what a naive index pairing would produce on this same
// fixture, asserted to differ. Without it the test would pass against an
// index-paired implementation whenever the fixture happened to line up, which is
// precisely how #489 hid.
func TestCorrelationDoesNotPairByIndex(t *testing.T) {
	cues := divergentCues()
	got, err := parseRichSyncBody(richSyncBody(t, divergentEntriesJSON), cues)
	if err != nil {
		t.Fatalf("parseRichSyncBody: %v", err)
	}

	var entries []richSyncEntry
	if err := json.Unmarshal([]byte(divergentEntriesJSON), &entries); err != nil {
		t.Fatalf("decoding the fixture for the control: %v", err)
	}
	// Each chunk's text is unique across the fixture, so a timing traces back to
	// exactly one entry.
	byText := map[string]richSyncEntry{}
	for _, e := range entries {
		byText[e.X] = e
	}

	// (1) Every emitted timing points at a cue within tolerance of ITS OWN
	// entry's ts -- the property index pairing cannot hold. (3) and no two
	// entries bind the same cue.
	seenLine := map[int]string{}
	for _, wt := range got {
		e, ok := byText[wt.Text]
		if !ok {
			t.Fatalf("emitted a timing %q that traces to no fixture entry", wt.Text)
		}
		cueMS := int(cues[wt.Line].Time.Total * 1000)
		if diff := cueMS - int(e.TS*1000); diff > richSyncBindToleranceMS || diff < -richSyncBindToleranceMS {
			t.Errorf("entry %q (ts %.2f) bound to cue %d at %d ms, %d ms away; tolerance is %d",
				wt.Text, e.TS, wt.Line, cueMS, diff, richSyncBindToleranceMS)
		}
		if prev, dup := seenLine[wt.Line]; dup && prev != wt.Text {
			t.Errorf("cue %d bound by both %q and %q; a cue may be bound once", wt.Line, prev, wt.Text)
		}
		seenLine[wt.Line] = wt.Text
	}

	// (2) The unbindable entries emit NOTHING. "delta" and "foxtrot" have no cue
	// in range; "echo" sits between two candidates and matches neither's text.
	for _, dropped := range []string{"delta", "foxtrot", "echo"} {
		for _, wt := range got {
			if wt.Text == dropped {
				t.Errorf("entry %q has no unambiguous cue but emitted a timing on line %d", dropped, wt.Line)
			}
		}
	}

	// The rule's outcome on this fixture, stated so a change to it is a visible
	// test edit rather than silent drift.
	want := map[int]string{0: "alpha", 2: "charlie"}
	if len(seenLine) != len(want) {
		t.Fatalf("bound %d cues (%v), want %d (%v)", len(seenLine), seenLine, len(want), want)
	}
	for line, text := range want {
		if seenLine[line] != text {
			t.Errorf("cue %d bound to %q, want %q", line, seenLine[line], text)
		}
	}

	// --- NEGATIVE CONTROL ---
	// What a naive index pairing (entry i -> cue i) would have produced here. If
	// the timestamp rule ever equals it, this test has stopped discriminating.
	indexPaired := map[int]string{}
	for i, e := range entries {
		if i < len(cues) {
			indexPaired[i] = e.X
		}
	}
	same := len(indexPaired) == len(seenLine)
	for line, text := range indexPaired {
		if seenLine[line] != text {
			same = false
		}
	}
	if same {
		t.Fatal("the timestamp rule produced the SAME assignment an index pairing would have. " +
			"This fixture no longer discriminates, so the test cannot catch #489's mistake")
	}

	// Name the discriminating line concretely: entry index 1 ("alpha") is at
	// ts 10.00 and belongs to cue 0, but index pairing would give it cue 1 -- a
	// cue ten seconds away carrying different text.
	if indexPaired[1] != "alpha" {
		t.Fatalf("fixture drift: index pairing no longer assigns %q to cue 1", "alpha")
	}
	if seenLine[1] == "alpha" {
		t.Error(`entry "alpha" bound to cue 1, which is where an INDEX pairing would put it; ` +
			"ts 10.00 corresponds to cue 0")
	}
}
