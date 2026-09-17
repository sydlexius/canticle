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
	// The scalar cases below were never the dangerous ones. EVERY field is
	// optional to encoding/json, so a body that is a non-empty ARRAY OF OBJECTS
	// whose keys the provider renamed decodes without error into N entries of
	// {TS:0, L:nil} -- and an entry-count emptiness test sees N, calls it a
	// success, and returns zero timings with nil error. That is precisely the
	// silent payload change this sentinel exists to name, so the class has to be
	// enumerated here and not just the scalars.
	renamed := []string{
		`[{}]`,
		`[null,null]`,
		`[{"start":1.0,"end":2.0,"text":"alpha","words":[{"chunk":"alpha","offset":0.0}]}]`,
		`[{"ts":1.0,"te":2.0,"x":"alpha","words":[{"c":"alpha","o":0.0}]}]`,
		`[{"ts":1.0,"te":2.0,"x":"alpha","l":[]}]`,
	}
	for _, inner := range append([]string{"null", "[]", "", "  ", "{}", `"nope"`, "0", "false", "[oops"}, renamed...) {
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
		// Errorf, NOT Fatalf: the negative control below is the half of this test
		// that discriminates index pairing from timestamp binding, and a Fatal
		// here returns before it ever runs -- under the exact mutation it exists
		// to catch. A test whose control is unreachable in the failing case is
		// not a control.
		t.Errorf("bound %d cues (%v), want %d (%v)", len(seenLine), seenLine, len(want), want)
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

// TestBindRefusesALoneCandidateWhoseTextDisagrees covers the false bind a
// single-candidate fast path allows.
//
// Entries bind in ascending ts and the first bind wins, so a slightly-early
// entry reaches a cue inside the window BEFORE the entry matching that cue
// exactly is considered: one candidate at the time of asking, two once the whole
// body is in view. Without a text check the early entry takes the cue and the
// exact match is dropped -- one line's words written onto another line's text,
// which is the wrong-content outcome this rule exists to refuse.
func TestBindRefusesALoneCandidateWhoseTextDisagrees(t *testing.T) {
	cues := []models.Lines{cue(20.0, "bravo")}
	raw := richSyncBody(t, `[
		{"ts":19.8,"te":19.9,"x":"alpha","l":[{"c":"alphaword","o":0.0}]},
		{"ts":20.0,"te":20.5,"x":"bravo","l":[{"c":"bravoword","o":0.0}]}
	]`)

	got, err := parseRichSyncBody(raw, cues)
	if err != nil {
		t.Fatalf("parseRichSyncBody: %v", err)
	}
	for _, w := range got {
		if w.Text == "alphaword" {
			t.Errorf(`entry "alpha" bound to the cue whose text is %q; a lone candidate `+
				`must not bind when the texts positively disagree`, cues[w.Line].Text)
		}
	}
	if len(got) != 1 || got[0].Text != "bravoword" {
		t.Errorf("got %+v; want only the exactly-matching entry bound", got)
	}
}

// TestBindStillBindsWhenEitherTextIsAbsent pins the other half of that rule.
// richsync's x is optional, so an empty text on either side is NO EVIDENCE and
// must leave the timestamp's verdict alone. Without this, adding the text check
// would silently stop binding every body that omits x.
func TestBindStillBindsWhenEitherTextIsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name, entries string
		cueText       string
	}{
		{"entry text absent", `[{"ts":20.0,"te":20.5,"l":[{"c":"word","o":0.0}]}]`, "bravo"},
		{"cue text absent", `[{"ts":20.0,"te":20.5,"x":"bravo","l":[{"c":"word","o":0.0}]}]`, ""},
		{"both absent", `[{"ts":20.0,"te":20.5,"l":[{"c":"word","o":0.0}]}]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRichSyncBody(richSyncBody(t, tc.entries), []models.Lines{cue(20.0, tc.cueText)})
			if err != nil {
				t.Fatalf("parseRichSyncBody: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d timings, want 1; an absent text is not a disagreement", len(got))
			}
		})
	}
}

// TestEndMSNeverPrecedesStartMS covers two provider shapes that invert the span:
// a last chunk whose offset runs past the entry's te, and chunks not ascending
// by o (the next chunk's start is read as this one's end without assuming that
// order). Latent only because a2Words does not read EndMS today.
func TestEndMSNeverPrecedesStartMS(t *testing.T) {
	for _, tc := range []struct{ name, entries string }{
		{"last chunk starts after te", `[{"ts":1.0,"te":1.2,"x":"alpha","l":[{"c":"alpha","o":0.5}]}]`},
		{"chunks not ascending by offset", `[{"ts":1.0,"te":9.0,"x":"alpha","l":[{"c":"alpha","o":2.0},{"c":"bravo","o":0.5}]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRichSyncBody(richSyncBody(t, tc.entries), []models.Lines{cue(1.0, "alpha")})
			if err != nil {
				t.Fatalf("parseRichSyncBody: %v", err)
			}
			for _, w := range got {
				if w.EndMS < w.StartMS {
					t.Errorf("%q: EndMS %d precedes StartMS %d; a word cannot have negative length",
						w.Text, w.EndMS, w.StartMS)
				}
			}
		})
	}
}

// TestUnitSanityCatchesASparseMillisecondBody covers the gap a purely RELATIVE
// bound leaves. The ratio arm's sensitivity scales with how much of the track the
// body COVERS: words spanning the first tenth of a long track give a ratio ten
// times smaller than full coverage carrying the identical error, so a sparse body
// slips a genuine 1000x past a bound a complete one would trip.
//
// Here the last cue is at 190s while a millisecond-valued body's true content
// spans 0..15s, so the body covers a small fraction of the cue span and the ratio
// arm is correspondingly less sensitive. The 1000x error is still rejected, by
// the factor arm (190000*4), which is what decides at this cue span; the additive
// arm is covered separately below.
func TestUnitSanityCatchesASparseMillisecondBody(t *testing.T) {
	cues := make([]models.Lines, 0, 20)
	for i := range 20 {
		cues = append(cues, cue(float64(i)*10.0, "alpha"))
	}
	// 15000.0 "seconds" is 15s expressed in milliseconds: the 1000x error.
	raw := richSyncBody(t, `[{"ts":15000.0,"te":15100.0,"x":"alpha","l":[{"c":"alpha","o":0.0}]}]`)

	_, err := parseRichSyncBody(raw, cues)
	if !errors.Is(err, ErrUnparsableRichSyncBody) {
		t.Fatalf("error = %v; want ErrUnparsableRichSyncBody. A 1000x unit error over a sparsely "+
			"covered track must not pass merely because the ratio arm is less sensitive there", err)
	}
}

// TestUnitSanityAcceptsALegitimateLongRecording is the counterweight: the
// absolute ceiling must never reject a real long-form track (a DJ set, an
// audiobook chapter), or it trades a silent failure for a loud wrong answer.
func TestUnitSanityAcceptsALegitimateLongRecording(t *testing.T) {
	// A three-hour recording, cues sparse across it, word starts in seconds. The
	// entry sits ON its cue: this test is about the sanity check accepting the
	// body, so the bind must not fail for an unrelated tolerance reason.
	cues := []models.Lines{cue(0.0, "alpha"), cue(10800.0, "bravo")}
	raw := richSyncBody(t, `[{"ts":10800.0,"te":10801.0,"x":"bravo","l":[{"c":"bravo","o":0.0}]}]`)

	got, err := parseRichSyncBody(raw, cues)
	if err != nil {
		t.Fatalf("parseRichSyncBody rejected a legitimate 3-hour recording: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d timings, want 1", len(got))
	}
}

// TestUnitSanityAdditiveArmDecidesOnAShortCueSpan covers the arm the sparse test
// above does NOT reach, which CodeRabbit correctly flagged.
//
// The two arms swap at a 20s cue span: below it, lastCue*4 is smaller than
// lastCue+60s, so the ADDITIVE arm sets the bound. That is the whole reason it
// exists -- a multiple alone is meaninglessly tight on a short span (a cue at
// 0.2s would allow 0.8s), so ordinary trailing content past the last sung line
// would trip it. The first case proves the slack accepts that trailing content;
// the second proves it still rejects a unit error.
func TestUnitSanityAdditiveArmDecidesOnAShortCueSpan(t *testing.T) {
	// Cues end at 5s, so factor=20000 but additive=65000: the additive arm binds.
	cues := []models.Lines{cue(0.0, "alpha"), cue(5.0, "bravo")}

	t.Run("accepts trailing content the factor arm alone would reject", func(t *testing.T) {
		// A word at 30s: past lastCue*4 (20s), inside lastCue+60s (65s). A jingle
		// or outro running well past the last sung line looks exactly like this.
		raw := richSyncBody(t, `[{"ts":5.0,"te":31.0,"x":"bravo","l":[{"c":"bravo","o":25.0}]}]`)
		if _, err := parseRichSyncBody(raw, cues); err != nil {
			t.Fatalf("rejected legitimate trailing content: %v. The additive slack exists so a "+
				"short cue span does not make the bound meaninglessly tight", err)
		}
	})

	t.Run("still rejects a unit error", func(t *testing.T) {
		// 5000.0 "seconds" is 5s in milliseconds: the 1000x error, far past both arms.
		raw := richSyncBody(t, `[{"ts":5000.0,"te":5100.0,"x":"bravo","l":[{"c":"bravo","o":0.0}]}]`)
		if _, err := parseRichSyncBody(raw, cues); !errors.Is(err, ErrUnparsableRichSyncBody) {
			t.Fatalf("error = %v; want ErrUnparsableRichSyncBody", err)
		}
	})
}

// TestParseRejectsRenamedCHUNKKeys is the level the entry-key guard missed.
//
// An entry-key rename is caught because the entries decode with no chunks. A
// CHUNK-key rename decodes the right NUMBER of chunks, every one empty, so a
// structural count passes and the parser emits timings whose Text is "" and whose
// stamps are all the line start -- garbage presented as data, which is strictly
// worse than the error it should have been.
func TestParseRejectsRenamedCHUNKKeys(t *testing.T) {
	cues := []models.Lines{cue(1.0, "alpha")}
	raw := richSyncBody(t, `[{"ts":1.0,"te":2.0,"x":"alpha","l":[
		{"chunk":"alpha","offset":0.0},{"chunk":"bravo","offset":0.5}]}]`)

	got, err := parseRichSyncBody(raw, cues)
	if !errors.Is(err, ErrUnparsableRichSyncBody) {
		t.Fatalf("error = %v (emitted %d timings); want ErrUnparsableRichSyncBody. Chunks that "+
			"decode to empty text are a payload shape change, not word data", err, len(got))
	}
}
