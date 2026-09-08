package innertube

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

func loadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return raw
}

func TestDecode_FixtureBrowse(t *testing.T) {
	raw := loadTestdata(t, "browse.json")

	song, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: unexpected error: %v", err)
	}

	const wantCueCount = 22
	lines := song.Subtitles.Lines
	if len(lines) != wantCueCount {
		t.Fatalf("cue count = %d, want %d", len(lines), wantCueCount)
	}

	first := lines[0]
	if first.Time.Total != 0 {
		t.Errorf("first cue Total = %v, want 0", first.Time.Total)
	}
	last := lines[len(lines)-1]
	if last.Time.Total <= first.Time.Total {
		t.Errorf("last cue Total = %v, not after first cue Total = %v", last.Time.Total, first.Time.Total)
	}
	// The fixture's final cue starts at 114550ms; MsToTime must be internally
	// consistent (Minutes/Seconds/Hundredths agree with Total, per #863).
	const wantLastMs = 114550
	wantTime := 114.55
	if last.Time.Total != wantTime {
		t.Errorf("last cue Total = %v, want %v", last.Time.Total, wantTime)
	}
	if last.Time.Minutes != wantLastMs/60000 {
		t.Errorf("last cue Minutes = %d, want %d", last.Time.Minutes, wantLastMs/60000)
	}
}

func TestDecode_FixtureCuesSortedMonotonic(t *testing.T) {
	raw := loadTestdata(t, "browse.json")

	song, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: unexpected error: %v", err)
	}

	lines := song.Subtitles.Lines
	for i := 1; i < len(lines); i++ {
		if lines[i].Time.Total < lines[i-1].Time.Total {
			t.Fatalf("cues not monotonic at index %d: %v then %v", i, lines[i-1].Time.Total, lines[i].Time.Total)
		}
	}
}

func TestDecode_NoLyricsSection_WrapsErrNotFound(t *testing.T) {
	// Structurally valid JSON with no timedLyricsData at all -- a clean miss,
	// not a malformed response.
	raw := []byte(`{"contents":{}}`)

	_, err := Decode(raw)
	if err == nil {
		t.Fatal("Decode: expected an error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Decode error = %v, want wrapping ErrNotFound", err)
	}
}

func TestDecode_EmptyCueList_WrapsErrNotFound(t *testing.T) {
	raw := []byte(`{"contents":{"elementRenderer":{"newElement":{"type":{"componentType":{"model":{"timedLyricsModel":{"lyricsData":{"timedLyricsData":[]}}}}}}}}}`)

	_, err := Decode(raw)
	if err == nil {
		t.Fatal("Decode: expected an error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Decode error = %v, want wrapping ErrNotFound", err)
	}
}

func TestDecode_MalformedJSON_DoesNotWrapErrNotFound(t *testing.T) {
	raw := []byte(`{not valid json`)

	_, err := Decode(raw)
	if err == nil {
		t.Fatal("Decode: expected an error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("Decode error = %v, must NOT wrap ErrNotFound (this is a transport-class failure)", err)
	}
}

func TestDecode_MalformedTimestamp_DoesNotWrapErrNotFound(t *testing.T) {
	// A cue is present (not a miss) but its timestamp field is structurally
	// wrong -- unparsable as a number. This must classify as transport, not
	// as a benign miss.
	raw := []byte(`{"contents":{"elementRenderer":{"newElement":{"type":{"componentType":{"model":{"timedLyricsModel":{"lyricsData":{"timedLyricsData":[{"lyricLine":"x","cueRange":{"startTimeMilliseconds":"not-a-number","endTimeMilliseconds":"0"}}]}}}}}}}}}`)

	_, err := Decode(raw)
	if err == nil {
		t.Fatal("Decode: expected an error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("Decode error = %v, must NOT wrap ErrNotFound (this is a transport-class failure)", err)
	}
}

// browseWithCues builds a minimal, structurally valid browse payload string
// carrying the given raw timedLyricsData entries verbatim, so tests can
// control cue shape precisely without depending on the fixture.
func browseWithCues(rawCues string) []byte {
	return []byte(`{"contents":{"elementRenderer":{"newElement":{"type":{"componentType":{"model":{"timedLyricsModel":{"lyricsData":{"timedLyricsData":[` + rawCues + `]}}}}}}}}}`)
}

// cueJSON marshals its fields through encoding/json rather than string
// concatenation, so a text value containing a quote or backslash still
// produces valid JSON instead of a broken fixture that fails with a
// confusing decode error unrelated to whatever the test is actually
// asserting.
func cueJSON(text, startMs, endMs string) string {
	cue := struct {
		LyricLine string `json:"lyricLine"`
		CueRange  struct {
			StartTimeMilliseconds string `json:"startTimeMilliseconds"`
			EndTimeMilliseconds   string `json:"endTimeMilliseconds"`
		} `json:"cueRange"`
	}{LyricLine: text}
	cue.CueRange.StartTimeMilliseconds = startMs
	cue.CueRange.EndTimeMilliseconds = endMs

	b, err := json.Marshal(cue)
	if err != nil {
		panic(fmt.Sprintf("cueJSON: marshal: %v", err))
	}
	return string(b)
}

// TestDecode_LeadingBracketedTextNotFabricatedIntoCue guards 852-F1: a cue
// whose TEXT happens to begin with a bracketed, timestamp-shaped token must
// pass through as ONE cue, never split into an extra fabricated cue. This is
// the InnerTube-specific case lrcnormalize.Expand would have mishandled --
// text and timing are separate sibling fields here, so a text-only pattern
// match has no business changing cue count.
func TestDecode_LeadingBracketedTextNotFabricatedIntoCue(t *testing.T) {
	raw := browseWithCues(strings.Join([]string{
		cueJSON("line-alpha", "0", "1000"),
		cueJSON("[00:05.00]line-beta", "2000", "3000"),
		cueJSON("line-gamma", "4000", "5000"),
	}, ","))

	song, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: unexpected error: %v", err)
	}

	const wantCueCount = 3
	lines := song.Subtitles.Lines
	if len(lines) != wantCueCount {
		t.Fatalf("cue count = %d, want %d (a bracketed-text token must not fabricate an extra cue)", len(lines), wantCueCount)
	}
	if lines[1].Text != "[00:05.00]line-beta" {
		t.Errorf("lines[1].Text = %q, want the bracketed token kept verbatim in the text", lines[1].Text)
	}
}

// TestDecode_OutOfOrderCuesSortedMonotonic guards the F1 replacement
// mechanism: cues arriving out of order must still come out sorted by
// Time.Total, since the sort responsibility moved from lrcnormalize.Expand
// to a direct sort in Decode.
func TestDecode_OutOfOrderCuesSortedMonotonic(t *testing.T) {
	raw := browseWithCues(strings.Join([]string{
		cueJSON("line-third", "9000", "9500"),
		cueJSON("line-first", "1000", "1500"),
		cueJSON("line-second", "5000", "5500"),
	}, ","))

	song, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: unexpected error: %v", err)
	}

	lines := song.Subtitles.Lines
	if len(lines) != 3 {
		t.Fatalf("cue count = %d, want 3", len(lines))
	}
	wantOrder := []string{"line-first", "line-second", "line-third"}
	for i, want := range wantOrder {
		if lines[i].Text != want {
			t.Errorf("lines[%d].Text = %q, want %q", i, lines[i].Text, want)
		}
	}
	for i := 1; i < len(lines); i++ {
		if lines[i].Time.Total < lines[i-1].Time.Total {
			t.Fatalf("cues not monotonic at index %d: %v then %v", i, lines[i-1].Time.Total, lines[i].Time.Total)
		}
	}
}

// TestDecode_AllEmptyText_WrapsErrNotFound guards 852-F3: cues with valid
// timings but entirely empty text must classify as a clean miss (wrapping
// ErrNotFound), not a success -- a success retires the queue row and blocks
// another provider lane from answering.
func TestDecode_AllEmptyText_WrapsErrNotFound(t *testing.T) {
	raw := browseWithCues(strings.Join([]string{
		cueJSON("", "0", "1000"),
		cueJSON("   ", "1000", "2000"),
	}, ","))

	_, err := Decode(raw)
	if err == nil {
		t.Fatal("Decode: expected an error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Decode error = %v, want wrapping ErrNotFound", err)
	}
}

// TestDecode_WhitespaceOnlyCueTreatedAsEmpty guards 852-R2F3: a whitespace-
// only cue must be treated as empty text by the all-empty classifier, same as
// an explicit empty string, since it is trimmed at Cue construction.
func TestDecode_WhitespaceOnlyCueTreatedAsEmpty(t *testing.T) {
	raw := browseWithCues(cueJSON("   ", "0", "1000"))

	_, err := Decode(raw)
	if err == nil {
		t.Fatal("Decode: expected an error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Decode error = %v, want wrapping ErrNotFound", err)
	}
}

// TestDecode_PartialWhitespaceCueKeepsEveryCue guards 852-R2F3: a set with
// ONE real cue and one whitespace-only cue is a PARTIAL miss, not an
// all-empty one, so every cue must survive -- none dropped, the whitespace
// cue's text trimmed to empty rather than removed.
func TestDecode_PartialWhitespaceCueKeepsEveryCue(t *testing.T) {
	raw := browseWithCues(strings.Join([]string{
		cueJSON("line-real", "0", "1000"),
		cueJSON("   ", "1000", "2000"),
	}, ","))

	song, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: unexpected error: %v", err)
	}

	const wantCueCount = 2
	lines := song.Subtitles.Lines
	if len(lines) != wantCueCount {
		t.Fatalf("cue count = %d, want %d (a partial miss must not drop any cue)", len(lines), wantCueCount)
	}
	if lines[0].Text != "line-real" {
		t.Errorf("lines[0].Text = %q, want %q", lines[0].Text, "line-real")
	}
	if lines[1].Text != "" {
		t.Errorf("lines[1].Text = %q, want empty (whitespace-only cue trimmed)", lines[1].Text)
	}
}

// TestExtractCues_NegativeStartMs_DoesNotWrapErrNotFound guards 852-F4: a
// negative timestamp is a malformed payload, transport-class, never a benign
// miss -- and must be rejected the same way at both exported entry points
// rather than silently clamped downstream.
func TestExtractCues_NegativeStartMs_DoesNotWrapErrNotFound(t *testing.T) {
	raw := browseWithCues(cueJSON("line-alpha", "-5000", "1000"))

	_, err := ExtractCues(raw)
	if err == nil {
		t.Fatal("ExtractCues: expected an error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("ExtractCues error = %v, must NOT wrap ErrNotFound (this is a transport-class failure)", err)
	}
}

// TestExtractCues_NegativeEndMs_DoesNotWrapErrNotFound guards 852-R2F2 case
// 1: a negative endTimeMilliseconds is a malformed payload, transport-class,
// with a VALID start -- so this exercises the endMs<0 branch on its own,
// distinct from TestExtractCues_NegativeStartMs_DoesNotWrapErrNotFound above.
func TestExtractCues_NegativeEndMs_DoesNotWrapErrNotFound(t *testing.T) {
	raw := browseWithCues(cueJSON("line-alpha", "1000", "-5000"))

	_, err := ExtractCues(raw)
	if err == nil {
		t.Fatal("ExtractCues: expected an error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("ExtractCues error = %v, must NOT wrap ErrNotFound (this is a transport-class failure)", err)
	}
}

// TestDecode_DuplicateTimestampsPreserveOriginalOrder guards 852-R2F2 case 2:
// Decode sorts lines by Time.Total, and that sort must be STABLE so cues
// sharing the same timestamp come out in the provider's original order
// rather than an arbitrary one. The set below packs many cues into a few
// duplicate-timestamp groups, which is large enough to make an unstable sort
// visibly reorder ties.
func TestDecode_DuplicateTimestampsPreserveOriginalOrder(t *testing.T) {
	const groups = 3
	const perGroup = 8
	entries := make([]string, 0, groups*perGroup)
	wantOrder := make([]string, 0, groups*perGroup)
	// Interleave so the input is not already grouped by timestamp -- that is
	// what forces a real sort rather than a no-op.
	for round := 0; round < perGroup; round++ {
		for g := 0; g < groups; g++ {
			startMs := (g + 1) * 1000
			text := fmt.Sprintf("g%d-item%d", g, round)
			entries = append(entries, cueJSON(text, strconv.Itoa(startMs), strconv.Itoa(startMs+500)))
		}
	}
	for g := 0; g < groups; g++ {
		for round := 0; round < perGroup; round++ {
			wantOrder = append(wantOrder, fmt.Sprintf("g%d-item%d", g, round))
		}
	}

	raw := browseWithCues(strings.Join(entries, ","))
	song, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: unexpected error: %v", err)
	}

	lines := song.Subtitles.Lines
	if len(lines) != len(wantOrder) {
		t.Fatalf("cue count = %d, want %d", len(lines), len(wantOrder))
	}
	// Within each timestamp group, the relative order of the original text
	// values must be preserved.
	gotByGroup := make(map[int][]string, groups)
	for _, l := range lines {
		g := int(l.Time.Total) - 1 // startMs == (g+1)*1000, so Total == g+1 seconds
		gotByGroup[g] = append(gotByGroup[g], l.Text)
	}
	for g := 0; g < groups; g++ {
		var want []string
		for round := 0; round < perGroup; round++ {
			want = append(want, fmt.Sprintf("g%d-item%d", g, round))
		}
		got := gotByGroup[g]
		if len(got) != len(want) {
			t.Fatalf("group %d: got %d cues, want %d", g, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("group %d: order not preserved: got %v, want %v", g, got, want)
			}
		}
	}
}

// TestExtractCues_EndBeforeStartMs_DoesNotWrapErrNotFound guards 871-C4: an
// endTimeMilliseconds earlier than its own startTimeMilliseconds is a
// malformed payload -- transport-class, never a benign miss -- the same
// treatment as the negative-start and negative-end cases above.
func TestExtractCues_EndBeforeStartMs_DoesNotWrapErrNotFound(t *testing.T) {
	raw := browseWithCues(cueJSON("line-alpha", "5000", "1000"))

	_, err := ExtractCues(raw)
	if err == nil {
		t.Fatal("ExtractCues: expected an error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("ExtractCues error = %v, must NOT wrap ErrNotFound (this is a transport-class failure)", err)
	}
}

// TestExtractCues_ZeroLengthCueAccepted guards the zero-length-cue decision
// documented at the endMs<startMs check in decode.go: endMs == startMs is a
// legitimate degenerate case (a single-instant cue), not malformed, and must
// still be accepted.
func TestExtractCues_ZeroLengthCueAccepted(t *testing.T) {
	raw := browseWithCues(cueJSON("line-alpha", "1000", "1000"))

	cues, err := ExtractCues(raw)
	if err != nil {
		t.Fatalf("ExtractCues: unexpected error for a zero-length cue: %v", err)
	}
	if len(cues) != 1 || cues[0].StartMs != 1000 || cues[0].EndMs != 1000 {
		t.Errorf("ExtractCues: got %+v, want one cue with StartMs == EndMs == 1000", cues)
	}
}

// TestExtractCues_AscendingCuesStillAccepted is the C4 non-regression check:
// ordinary cues whose end follows their start must still decode cleanly
// after the endMs<startMs rejection was added.
func TestExtractCues_AscendingCuesStillAccepted(t *testing.T) {
	raw := browseWithCues(strings.Join([]string{
		cueJSON("line-alpha", "0", "1000"),
		cueJSON("line-beta", "1000", "2500"),
	}, ","))

	cues, err := ExtractCues(raw)
	if err != nil {
		t.Fatalf("ExtractCues: unexpected error: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("ExtractCues: got %d cues, want 2", len(cues))
	}
}

// TestCueJSON_QuoteAndBackslashRoundTrip guards 871-C3: the cueJSON test
// helper must escape its inputs so a value containing a quote or backslash
// still produces valid JSON, rather than corrupting the fixture.
func TestCueJSON_QuoteAndBackslashRoundTrip(t *testing.T) {
	const text = `she said "back\slash" and quit`
	raw := browseWithCues(cueJSON(text, "0", "1000"))

	cues, err := ExtractCues(raw)
	if err != nil {
		t.Fatalf("ExtractCues: unexpected error decoding a quoted/backslashed value: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("ExtractCues: got %d cues, want 1", len(cues))
	}
	if cues[0].Text != text {
		t.Errorf("cues[0].Text = %q, want %q", cues[0].Text, text)
	}
}

func TestExtractCues_SiblingShape(t *testing.T) {
	// Regression guard for the measured sibling trap: cueRange sits beside
	// lyricLine, not nested inside it. A struct that (incorrectly) nested
	// cueRange under lyricLine would find zero timings against this fixture.
	raw := loadTestdata(t, "browse.json")

	cues, err := ExtractCues(raw)
	if err != nil {
		t.Fatalf("ExtractCues: unexpected error: %v", err)
	}
	if len(cues) == 0 {
		t.Fatal("ExtractCues: got zero cues, want 22")
	}
	for i, c := range cues {
		if c.StartMs == 0 && c.EndMs == 0 && i > 0 {
			t.Errorf("cue %d: StartMs and EndMs both zero, timings not extracted", i)
		}
	}
	// EndMs must be carried, not dropped -- a genuine distinguishing feature
	// of this provider's payload (see types.go Cue.EndMs).
	if cues[0].EndMs != 5070 {
		t.Errorf("cues[0].EndMs = %d, want 5070", cues[0].EndMs)
	}
}

// TestUpstreamToken pins the CLOSED SET (#859). The mapping is a closed switch
// to a package constant, never a passthrough: an unrecognized value yields "",
// and the writer then omits the [upstream:] tag entirely.
//
// The recognized inputs are the EXACT strings measured live against the public
// browse endpoint on 2026-09-07 -- "Source: Musixmatch" and "Source: LyricFind",
// both prefixed and mixed-case. Neither the prefix nor the casing was in the
// design doc; both came from the capture, and a mapping written from the doc
// alone would have failed on both.
func TestUpstreamToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		// Measured verbatim.
		{"captured musixmatch", "Source: Musixmatch", UpstreamMusixmatch},
		{"captured lyricfind", "Source: LyricFind", UpstreamLyricFind},

		// Tolerated variation around the same two names.
		{"no prefix", "Musixmatch", UpstreamMusixmatch},
		{"lowercased", "source: lyricfind", UpstreamLyricFind},
		{"surrounding space", "  Source: Musixmatch  ", UpstreamMusixmatch},

		// The default arm. Each of these must yield NO tag rather than a
		// passthrough, and the reason is concrete: the fetch-time writer
		// formats this token straight into an LRC header with fmt.Sprintf and
		// does NOT sanitize it, so a newline would truncate the tag and end the
		// header block early.
		{"absent", "", ""},
		{"whitespace only", "   ", ""},
		{"prefix but no name", "Source:", ""},
		{"an upstream we do not know", "Source: Placeholder Licensor", ""},
		{"a renamed upstream", "Source: Musixmatch Inc.", ""},
		{"newline injection", "Source: Musixmatch\n[source:evil]", ""},
		{"carriage return injection", "Source: Musixmatch\r[source:evil]", ""},
		{"bracket injection", "Source: Musixmatch][ti:evil]", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamToken(tc.in); got != tc.want {
				t.Errorf("upstreamToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExtractUpstream_FromTheCapturedShape drives the extraction through the
// REAL nested path rather than the mapping alone. The path is the fragile part
// -- it is uncaptured in the trimmed browse fixture, which is why it was
// measured live -- so a test that only exercised upstreamToken would pass even
// if the struct tag were wrong and the field never populated.
func TestExtractUpstream_FromTheCapturedShape(t *testing.T) {
	// The shape is asserted here as a literal, matching the path measured at
	// contents.elementRenderer.newElement.type.componentType.model
	//   .timedLyricsModel.lyricsData.sourceMessage
	payload := func(msg string) []byte {
		return []byte(`{"contents":{"elementRenderer":{"newElement":{"type":{"componentType":{"model":{"timedLyricsModel":{"lyricsData":{"sourceMessage":` + msg + `}}}}}}}}}`)
	}

	if got := ExtractUpstream(payload(`"Source: Musixmatch"`)); got != UpstreamMusixmatch {
		t.Errorf("captured musixmatch shape: got %q, want %q -- the nested path or struct tag is wrong", got, UpstreamMusixmatch)
	}
	if got := ExtractUpstream(payload(`"Source: LyricFind"`)); got != UpstreamLyricFind {
		t.Errorf("captured lyricfind shape: got %q, want %q", got, UpstreamLyricFind)
	}

	// An instrumental returns lyricsData: null -- measured, not hypothesized.
	// The extraction must be null-safe at the PARENT, not merely at the leaf.
	nullParent := []byte(`{"contents":{"elementRenderer":{"newElement":{"type":{"componentType":{"model":{"timedLyricsModel":{"lyricsData":null}}}}}}}}`)
	if got := ExtractUpstream(nullParent); got != "" {
		t.Errorf("a null lyricsData must yield no upstream, got %q", got)
	}
	if got := ExtractUpstream([]byte(`{"contents":{}}`)); got != "" {
		t.Errorf("an absent field must yield no upstream, got %q", got)
	}
	if got := ExtractUpstream([]byte(`not json`)); got != "" {
		t.Errorf("unparsable input must yield no upstream rather than panicking, got %q", got)
	}
}

// TestSourceMessageText covers the coercion the attribution field goes through
// before upstreamToken sees it (review finding F3).
//
// The field is decoded as json.RawMessage rather than string so that a
// surprising shape costs the ATTRIBUTION and never the CUES -- see
// upstreamPayload. These rows pin both the shapes that must be read and the
// shapes that must degrade quietly.
func TestSourceMessageText(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"bare string, the measured shape", `"Source: Musixmatch"`, "Source: Musixmatch"},
		// A display string split at a styling boundary. Taking runs[0] alone
		// would truncate this to "Source: " and lose the licensor entirely,
		// which is why the runs are concatenated.
		{"runs split across styling boundaries", `{"runs":[{"text":"Source: "},{"text":"LyricFind"}]}`, "Source: LyricFind"},
		{"single run", `{"runs":[{"text":"Source: Musixmatch"}]}`, "Source: Musixmatch"},
		{"empty runs array", `{"runs":[]}`, ""},

		// Every remaining shape degrades to "" -- no tag, which asserts nothing.
		{"absent", ``, ""},
		{"null", `null`, ""},
		{"numeric", `42`, ""},
		{"array", `["Source: Musixmatch"]`, ""},
		{"object without runs", `{"text":"Source: Musixmatch"}`, ""},
		{"malformed", `{oops`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceMessageText([]byte(tc.raw)); got != tc.want {
				t.Errorf("sourceMessageText(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestExtractCues_SurvivesAnySourceMessageShape is the REGRESSION test for F3,
// and it is about the cue path rather than the attribution.
//
// The two fields are siblings on the wire, and decoding them on ONE struct
// coupled them fatally: encoding/json aborts the whole unmarshal on a single
// type mismatch, so a non-string sourceMessage returned ZERO cues and a
// transport-class error (which does not degrade to a benign miss) even though
// the payload carried a perfectly valid timedLyricsData array. Measured before
// the fix: `runs-object -> 0 cues`.
func TestExtractCues_SurvivesAnySourceMessageShape(t *testing.T) {
	payload := func(sourceMessage string) []byte {
		return []byte(`{"contents":{"elementRenderer":{"newElement":{"type":{"componentType":{"model":{"timedLyricsModel":{"lyricsData":{"sourceMessage":` +
			sourceMessage +
			`,"timedLyricsData":[{"lyricLine":"placeholder","cueRange":{"startTimeMilliseconds":"0","endTimeMilliseconds":"100"}}]}}}}}}}}}`)
	}
	for _, tc := range []struct{ name, raw, wantUpstream string }{
		{"bare string", `"Source: LyricFind"`, UpstreamLyricFind},
		{"runs object", `{"runs":[{"text":"Source: "},{"text":"LyricFind"}]}`, UpstreamLyricFind},
		{"numeric", `42`, ""},
		{"null", `null`, ""},
		{"nested object", `{"unexpected":{"deeply":"nested"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := payload(tc.raw)
			cues, err := ExtractCues(raw)
			if err != nil {
				t.Fatalf("ExtractCues returned %v -- the ATTRIBUTION field must never be able to fail the CUE path", err)
			}
			if len(cues) != 1 {
				t.Errorf("got %d cues, want 1: the payload carried a valid timedLyricsData array", len(cues))
			}
			if got := ExtractUpstream(raw); got != tc.wantUpstream {
				t.Errorf("ExtractUpstream = %q, want %q", got, tc.wantUpstream)
			}
		})
	}
}

// TestExtractCues_UntimedPayloadIsNotATransportFailure covers the MEASURED
// production defect: YouTube Music serves two shapes under timedLyricsData,
// and only one carries cueRange.
//
// Captured live 2026-09-08 against three of the five tracks that failed in
// production on v1.37.0: browse returned 17, 52 and 25 entries whose keyset
// was exactly {lyricLine} -- cueRange ABSENT on every one -- alongside
// `"sourceMessage": "Source: LyricFind"`. Go leaves the nested struct at its
// zero value for an absent object, so startTimeMilliseconds read "" and
// strconv.Atoi failed, returning an UNWRAPPED error. That classed a response
// full of usable words as a transport failure and retired the row as `failed`.
//
// The fixtures could not have caught this: every one was captured from a
// timed response, so the code and its tests agreed with each other and both
// disagreed with the live API.
func TestExtractCues_UntimedPayloadIsNotATransportFailure(t *testing.T) {
	raw := browseWithCues(`{"lyricLine":"first line"},{"lyricLine":"second line"}`)

	_, err := ExtractCues(raw)
	if err == nil {
		t.Fatal("ExtractCues: expected ErrUntimedLyrics, got nil")
	}
	if !errors.Is(err, ErrUntimedLyrics) {
		t.Fatalf("ExtractCues error = %v, want it to wrap ErrUntimedLyrics", err)
	}
	// It must NOT read as a benign miss either: the response carried words,
	// so a caller that buckets on ErrNotFound would discard usable content.
	if errors.Is(err, ErrNotFound) {
		t.Errorf("ExtractCues error = %v, must NOT wrap ErrNotFound -- the payload carries usable lyrics", err)
	}
}

// TestDecode_UntimedPayloadYieldsUnsyncedLyrics is the behavior the fix
// exists to deliver: an untimed payload becomes an unsynced result the writer
// can emit as .txt, rather than being thrown away.
func TestDecode_UntimedPayloadYieldsUnsyncedLyrics(t *testing.T) {
	raw := browseWithCues(`{"lyricLine":"first line"},{"lyricLine":""},{"lyricLine":"third line"}`)

	song, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: unexpected error: %v", err)
	}
	if len(song.Subtitles.Lines) != 0 {
		t.Errorf("Subtitles.Lines = %d, want 0 -- an untimed payload has no timings to claim", len(song.Subtitles.Lines))
	}
	// The blank middle line is PRESERVED, not dropped: it is a stanza break in
	// the plain-text rendering, and this path has no timings whose positions
	// could be shifted by keeping it (the reason the timed path leaves a
	// partially-empty cue set alone does not apply, but the same conservatism
	// about not silently editing a provider's text does).
	want := "first line\n\nthird line"
	if song.Lyrics.LyricsBody != want {
		t.Errorf("LyricsBody = %q, want %q", song.Lyrics.LyricsBody, want)
	}
}

// TestDecode_UntimedPayloadCarriesUpstream guards that the attribution still
// lands on the unsynced path. The production sample was LyricFind-sourced, so
// this is the exact combination prod sees, not a synthetic pairing.
func TestDecode_UntimedPayloadCarriesUpstream(t *testing.T) {
	raw := []byte(`{"contents":{"elementRenderer":{"newElement":{"type":{"componentType":{"model":{"timedLyricsModel":{"lyricsData":{"sourceMessage":"Source: LyricFind","timedLyricsData":[{"lyricLine":"a line"}]}}}}}}}}}`)

	song, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: unexpected error: %v", err)
	}
	if song.Upstream != UpstreamLyricFind {
		t.Errorf("Upstream = %q, want %q", song.Upstream, UpstreamLyricFind)
	}
}

// TestDecode_UntimedPayloadAllTextEmptyIsAMiss: an untimed payload whose every
// line is blank carries nothing usable, so it is a benign miss like its timed
// counterpart -- writing an empty .txt would retire the row and block another
// lane from answering.
func TestDecode_UntimedPayloadAllTextEmptyIsAMiss(t *testing.T) {
	raw := browseWithCues(`{"lyricLine":""},{"lyricLine":"   "}`)

	_, err := Decode(raw)
	if err == nil {
		t.Fatal("Decode: expected an error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Decode error = %v, want a benign miss wrapping ErrNotFound", err)
	}
}

// TestExtractCues_PartiallyTimedPayloadIsTransportClass: a MIX of timed and
// untimed entries is a shape neither branch can honestly serve. Treating it as
// timed would place the untimed lines at 00:00; treating it as plain would
// discard real timings. It has never been observed live, so it is rejected
// transport-class (retried) rather than retired as a miss.
func TestExtractCues_PartiallyTimedPayloadIsTransportClass(t *testing.T) {
	raw := browseWithCues(cueJSON("timed", "0", "100") + `,{"lyricLine":"untimed"}`)

	_, err := ExtractCues(raw)
	if err == nil {
		t.Fatal("ExtractCues: expected an error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("ExtractCues error = %v, must NOT wrap ErrNotFound", err)
	}
	if errors.Is(err, ErrUntimedLyrics) {
		t.Errorf("ExtractCues error = %v, must NOT wrap ErrUntimedLyrics -- a partial mix is not a plain-text payload", err)
	}
}

// TestExtractCues_PresentButMalformedCueRangeStaysTransportClass is the
// CONTROL for the tests above: the fix distinguishes an ABSENT cueRange from a
// present-but-broken one, and must not weaken the existing malformed-timestamp
// guard into the new plain-text branch.
func TestExtractCues_PresentButMalformedCueRangeStaysTransportClass(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"empty start", `{"lyricLine":"x","cueRange":{"startTimeMilliseconds":"","endTimeMilliseconds":"100"}}`},
		{"non-numeric start", cueJSON("x", "not-a-number", "100")},
		{"empty end", `{"lyricLine":"x","cueRange":{"startTimeMilliseconds":"0","endTimeMilliseconds":""}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExtractCues(browseWithCues(tc.raw))
			if err == nil {
				t.Fatal("ExtractCues: expected an error, got nil")
			}
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrUntimedLyrics) {
				t.Errorf("ExtractCues error = %v, want an unwrapped transport-class failure: cueRange was PRESENT and broken, not absent", err)
			}
		})
	}
}

// TestExtractCues_ExplicitNullCueRangeIsTransportClass covers the gap CR found
// on this branch: JSON `null` is an EXPLICIT value, not an absent field, and a
// pointer cannot tell the two apart -- both decode to nil.
//
// That collapses exactly the distinction this fix exists to draw. An absent
// cueRange is the licensor serving plain text (a real, measured shape); an
// explicit `"cueRange": null` is a payload asserting a timing field and then
// supplying nothing for it, which is malformed and has never been observed
// live. Treating the malformed one as plain text would settle a row from a
// payload nobody has ever seen, on the strength of a JSON encoding accident.
//
// Measured before the fix: `cueRange: null` returned ErrUntimedLyrics and
// Decode succeeded with an unsynced body.
func TestExtractCues_ExplicitNullCueRangeIsTransportClass(t *testing.T) {
	raw := browseWithCues(`{"lyricLine":"x","cueRange":null}`)

	_, err := ExtractCues(raw)
	if err == nil {
		t.Fatal("ExtractCues: expected an error, got nil")
	}
	if errors.Is(err, ErrUntimedLyrics) {
		t.Errorf("ExtractCues error = %v, must NOT wrap ErrUntimedLyrics: an explicit null is malformed, not a plain-text payload", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("ExtractCues error = %v, must NOT wrap ErrNotFound", err)
	}
}

// TestDecode_ExplicitNullCueRangeDoesNotYieldLyrics is the Decode-level twin:
// a malformed payload must not settle the row with an unsynced body.
func TestDecode_ExplicitNullCueRangeDoesNotYieldLyrics(t *testing.T) {
	raw := browseWithCues(`{"lyricLine":"x","cueRange":null}`)

	song, err := Decode(raw)
	if err == nil {
		t.Fatalf("Decode: expected an error, got nil (body=%q)", song.Lyrics.LyricsBody)
	}
	if song.Lyrics.LyricsBody != "" {
		t.Errorf("LyricsBody = %q, want empty: a malformed payload must not produce lyrics", song.Lyrics.LyricsBody)
	}
}

// TestExtractCues_MixedNullAndAbsentCueRange pins the interaction between the
// two nil sources. A payload mixing an absent cueRange with an explicit null is
// not the all-untimed shape, so it must not take the plain-text branch.
func TestExtractCues_MixedNullAndAbsentCueRange(t *testing.T) {
	raw := browseWithCues(`{"lyricLine":"a"},{"lyricLine":"b","cueRange":null}`)

	_, err := ExtractCues(raw)
	if err == nil {
		t.Fatal("ExtractCues: expected an error, got nil")
	}
	if errors.Is(err, ErrUntimedLyrics) || errors.Is(err, ErrNotFound) {
		t.Errorf("ExtractCues error = %v, want transport-class: an explicit null is present here, so this is not an all-untimed payload", err)
	}
}
