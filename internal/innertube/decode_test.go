package innertube

import (
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

func TestDecode_MonotonicAfterExpand(t *testing.T) {
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

func cueJSON(text, startMs, endMs string) string {
	return `{"lyricLine":"` + text + `","cueRange":{"startTimeMilliseconds":"` + startMs + `","endTimeMilliseconds":"` + endMs + `"}}`
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
