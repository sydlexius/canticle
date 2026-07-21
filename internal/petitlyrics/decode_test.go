package petitlyrics

import (
	"encoding/base64"
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/lrcnormalize"
	"github.com/sydlexius/canticle/internal/models"
)

// payloadFromFixture pulls the base64 lyricsData out of a fixture envelope and
// returns the decoded bytes. Fixtures are synthetic: they replicate the verified
// wire structure with placeholder text, so no provider content is vendored.
func payloadFromFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var parsed apiResponse
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	if len(parsed.Songs) == 0 {
		t.Fatalf("fixture %s carried no songs", name)
	}
	payload, err := base64.StdEncoding.DecodeString(parsed.Songs[0].LyricsData)
	if err != nil {
		t.Fatalf("base64 decode fixture %s: %v", name, err)
	}
	return payload
}

// TestClassifyPayload pins the payload-shape discriminator. This is the check
// that must not regress: the API's lyricsType field echoes the request and
// availableLyricsType is not a capability set, so the bytes are the only
// trustworthy signal for which tier actually came back.
func TestClassifyPayload(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    int
	}{
		{"plain text is unsynced", "type1_unsynced.xml", tierUnsynced},
		{"binary blob is line-sync", "type2_linesync.xml", tierLineSync},
		{"wsy XML is word-sync", "type3_wordsync.xml", tierWordSync},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPayload(payloadFromFixture(t, tc.fixture)); got != tc.want {
				t.Errorf("classifyPayload = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestClassifyPayload_Edges(t *testing.T) {
	if got := classifyPayload([]byte("  \n<wsy>\n</wsy>")); got != tierWordSync {
		t.Errorf("leading whitespace before <wsy> should still classify as word-sync, got %d", got)
	}
	if got := classifyPayload([]byte("plain\nlines\n")); got != tierUnsynced {
		t.Errorf("plain text should classify as unsynced, got %d", got)
	}
	if got := classifyPayload([]byte{'a', 0x00, 'b'}); got != tierLineSync {
		t.Errorf("NUL-bearing payload should classify as line-sync, got %d", got)
	}
	if got := classifyPayload([]byte{0xff, 0xfe, 0xfd}); got != tierLineSync {
		t.Errorf("invalid UTF-8 should classify as line-sync, got %d", got)
	}
}

func TestDecodeWordSync(t *testing.T) {
	cues, timings, err := decodeWordSync(payloadFromFixture(t, "type3_wordsync.xml"))
	if err != nil {
		t.Fatalf("decodeWordSync: %v", err)
	}
	if len(cues) != 3 {
		t.Fatalf("want 3 cues, got %d", len(cues))
	}
	// A line's cue timestamp is its FIRST word's start time.
	if cues[0].Time.Total != 3.790 {
		t.Errorf("cue[0] should take the first word's start time, got %v", cues[0].Time.Total)
	}
	if cues[0].Time.Seconds != 3 || cues[0].Time.Hundredths != 79 {
		t.Errorf("cue[0] time decomposition wrong: %+v", cues[0].Time)
	}
	if len(timings) != 9 {
		t.Fatalf("want 9 word timings, got %d", len(timings))
	}
	// Word timings must carry their owning line index so the A2 writer can group.
	if timings[0].Line != 0 || timings[3].Line != 1 {
		t.Errorf("word timings carry wrong line indices: %+v %+v", timings[0], timings[3])
	}
	// Assert CONCRETE start AND end values. A `w.EndMS >= w.StartMS` loop is not
	// enough: it passes even if EndMS is set from StartTime, which would silently
	// zero out every word's duration -- the whole point of the A2 payload.
	if timings[0].StartMS != 3790 || timings[0].EndMS != 4590 {
		t.Errorf("timing[0] = start %d end %d, want 3790/4590", timings[0].StartMS, timings[0].EndMS)
	}
	if timings[1].StartMS != 4600 || timings[1].EndMS != 4870 {
		t.Errorf("timing[1] = start %d end %d, want 4600/4870", timings[1].StartMS, timings[1].EndMS)
	}
	for i, w := range timings {
		if w.EndMS <= w.StartMS && w.Line != 2 {
			t.Errorf("timing[%d] has no duration: %+v", i, w)
		}
	}
}

// TestDecodeWordSync_OrderingIsStableThroughExpand pins the invariant that keeps
// WordTiming.Line meaningful. The caller runs these cues through
// lrcnormalize.Expand, which sorts them; indices assigned against an unsorted
// slice would then point at the wrong cue. decodeWordSync sorts first, so a
// payload whose lines arrive out of order still yields correct indices.
func TestDecodeWordSync_OrderingIsStableThroughExpand(t *testing.T) {
	doc := `<wsy>` +
		`<line><linestring>Later</linestring>` +
		`<word><starttime>30000</starttime><endtime>31000</endtime><wordstring>Later</wordstring></word></line>` +
		`<line><linestring>Earlier</linestring>` +
		`<word><starttime>1000</starttime><endtime>2000</endtime><wordstring>Earlier</wordstring></word></line>` +
		`</wsy>`
	cues, timings, err := decodeWordSync([]byte(doc))
	if err != nil {
		t.Fatalf("decodeWordSync: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("want 2 cues, got %d", len(cues))
	}
	if cues[0].Text != "Earlier" || cues[1].Text != "Later" {
		t.Fatalf("cues should be time-ordered, got %q then %q", cues[0].Text, cues[1].Text)
	}
	// Each timing must index the cue whose text it belongs to.
	for _, w := range timings {
		if cues[w.Line].Text != w.Text {
			t.Errorf("timing %q points at cue %q (line %d)", w.Text, cues[w.Line].Text, w.Line)
		}
	}
	// Expand must be a no-op on ordering here, or the indices break downstream.
	expanded := lrcnormalize.Expand(models.Synced{Lines: cues})
	for i := range cues {
		if expanded.Lines[i].Text != cues[i].Text {
			t.Errorf("Expand reordered cue %d: %q -> %q", i, cues[i].Text, expanded.Lines[i].Text)
		}
	}
}

// TestDecodeWordSync_ClampsNegativeTimings: a negative timestamp must be clamped
// the same way msToTime clamps the cue, so a cue and its word timings never
// disagree about the same word.
func TestDecodeWordSync_ClampsNegativeTimings(t *testing.T) {
	doc := `<wsy><line><linestring>Lorem</linestring>` +
		`<word><starttime>-5000</starttime><endtime>-1000</endtime><wordstring>Lorem</wordstring></word>` +
		`</line></wsy>`
	cues, timings, err := decodeWordSync([]byte(doc))
	if err != nil {
		t.Fatalf("decodeWordSync: %v", err)
	}
	if cues[0].Time.Total != 0 {
		t.Errorf("cue time should clamp to 0, got %v", cues[0].Time.Total)
	}
	if timings[0].StartMS != 0 || timings[0].EndMS != 0 {
		t.Errorf("word timings should clamp to 0 alongside the cue, got %+v", timings[0])
	}
}

// TestDecodeWordSync_PartialDegradation pins the observed real-world case where
// some lines have every word sharing one timestamp (10 of 86 on a verified
// track). Those lines are still emitted as cues; they are simply line-level.
func TestDecodeWordSync_PartialDegradation(t *testing.T) {
	_, timings, err := decodeWordSync(payloadFromFixture(t, "type3_wordsync.xml"))
	if err != nil {
		t.Fatalf("decodeWordSync: %v", err)
	}
	var line2 []WordTiming
	for _, w := range timings {
		if w.Line == 2 {
			line2 = append(line2, w)
		}
	}
	if len(line2) != 3 {
		t.Fatalf("want 3 words on the degraded line, got %d", len(line2))
	}
	for _, w := range line2 {
		if w.StartMS != line2[0].StartMS {
			t.Fatalf("degraded line should share one timestamp, got %+v", line2)
		}
	}
}

func TestDecodeWordSync_Rejects(t *testing.T) {
	if _, _, err := decodeWordSync([]byte("not xml at all")); err == nil {
		t.Error("malformed XML should error")
	}
	// A document with lines but no words has nothing to anchor a cue to.
	if _, _, err := decodeWordSync([]byte(`<wsy><line><linestring>x</linestring></line></wsy>`)); err == nil {
		t.Error("word-less lines should error rather than emit untimed cues")
	}
}

func TestDecodeUnsynced(t *testing.T) {
	got := decodeUnsynced(payloadFromFixture(t, "type1_unsynced.xml"))
	if got == "" {
		t.Fatal("unsynced payload decoded to empty string")
	}
	if got[len(got)-1] == '\n' {
		t.Error("trailing newlines should be trimmed")
	}
}

func TestMsToTime(t *testing.T) {
	tests := []struct {
		ms            int
		min, sec, hun int
		total         float64
	}{
		{0, 0, 0, 0, 0},
		{3790, 0, 3, 79, 3.79},
		{65432, 1, 5, 43, 65.432},
		{-5, 0, 0, 0, 0}, // negative clamps rather than producing a negative cue
	}
	for _, tc := range tests {
		got := msToTime(tc.ms)
		if got.Minutes != tc.min || got.Seconds != tc.sec || got.Hundredths != tc.hun {
			t.Errorf("msToTime(%d) = %dm%ds%dh, want %dm%ds%dh",
				tc.ms, got.Minutes, got.Seconds, got.Hundredths, tc.min, tc.sec, tc.hun)
		}
		if got.Total != tc.total {
			t.Errorf("msToTime(%d).Total = %v, want %v", tc.ms, got.Total, tc.total)
		}
	}
}
