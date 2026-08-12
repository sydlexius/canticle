package lrcnormalize

import (
	"strconv"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

func TestParseBody_TrimsCueText(t *testing.T) {
	// Cue text is trimmed (strings.TrimSpace), the shared convention across the
	// provider parse lanes that feed this normalizer.
	doc := ParseBody("[00:10.00]  Hello world  ")
	if len(doc.Cues) != 1 {
		t.Fatalf("want 1 cue, got %d", len(doc.Cues))
	}
	if doc.Cues[0].Text != "Hello world" {
		t.Errorf("want trimmed %q, got %q", "Hello world", doc.Cues[0].Text)
	}
}

func TestExpand_PreservesTotalOnlyCue(t *testing.T) {
	// A cue carrying only Time.Total (decomposed fields zero) and no embedded
	// stamp must pass through with its timestamp intact.
	in := models.Synced{Lines: []models.Lines{
		{Text: "x", Time: models.Time{Total: 65}},
	}}
	out := Expand(in)
	if len(out.Lines) != 1 {
		t.Fatalf("want 1 cue, got %d", len(out.Lines))
	}
	if out.Lines[0].Time.Total != 65 || out.Lines[0].Text != "x" {
		t.Errorf("total-only cue not preserved: %+v", out.Lines[0])
	}
}

func TestExpand_PreservesHighMinuteOuterCue(t *testing.T) {
	// Outer cue at 100:20 with an embedded 00:45 stamp: both survive, and the
	// 100-minute timestamp is not destroyed by a re-render round-trip.
	in := models.Synced{Lines: []models.Lines{
		{Text: "[00:45.00]Chorus", Time: models.Time{Minutes: 100, Seconds: 20, Total: 6020}},
	}}
	out := Expand(in)
	if len(out.Lines) != 2 {
		t.Fatalf("want 2 cues, got %d: %+v", len(out.Lines), out.Lines)
	}
	if out.Lines[0].Time.Total != 45 || out.Lines[1].Time.Total != 6020 {
		t.Errorf("want totals [45, 6020], got [%v, %v]", out.Lines[0].Time.Total, out.Lines[1].Time.Total)
	}
	if out.Lines[0].Text != "Chorus" || out.Lines[1].Text != "Chorus" {
		t.Errorf("text not shared/de-embedded: %+v", out.Lines)
	}
}

func TestExpand_SplitsEmbeddedTimestamp(t *testing.T) {
	// Parse-bug output: the second stamp is stranded in the first cue's text.
	in := models.Synced{Lines: []models.Lines{
		{Text: "[00:45.00]Chorus", Time: models.Time{Total: 12, Seconds: 12}},
	}}

	out := Expand(in)

	if len(out.Lines) != 2 {
		t.Fatalf("want 2 cues, got %d: %+v", len(out.Lines), out.Lines)
	}
	want := []string{"12|Chorus", "45|Chorus"}
	for i, w := range want {
		if got := fmtCue(out.Lines[i]); got != w {
			t.Errorf("cue %d: want %q, got %q", i, w, got)
		}
	}
}

func TestExpand_IdempotentOnCleanInput(t *testing.T) {
	in := models.Synced{Lines: []models.Lines{
		{Text: "A", Time: models.Time{Total: 10, Seconds: 10}},
		{Text: "B", Time: models.Time{Total: 20, Seconds: 20}},
	}}

	out := Expand(in)

	if len(out.Lines) != 2 {
		t.Fatalf("want 2 cues, got %d", len(out.Lines))
	}
	if fmtCue(out.Lines[0]) != "10|A" || fmtCue(out.Lines[1]) != "20|B" {
		t.Errorf("clean input changed: %+v", out.Lines)
	}
}

func TestParseBody_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCues []string // "total|text" per expected cue, in order
		wantTags int
	}{
		{
			name:     "millisecond precision truncates to hundredths",
			body:     "[00:12.345]word",
			wantCues: []string{"12.34|word"},
		},
		{
			name:     "music marker preserved as text",
			body:     "[00:05.00]♪",
			wantCues: []string{"5|♪"},
		},
		{
			name:     "orphan text and blank lines are dropped",
			body:     "orphan line with no timestamp\n\n[00:03.00]real cue",
			wantCues: []string{"3|real cue"},
		},
		{
			name:     "empty body yields nothing",
			body:     "",
			wantCues: nil,
		},
		{
			name:     "tags only, no cues",
			body:     "[ar:X]\n[ti:Y]",
			wantCues: nil,
			wantTags: 2,
		},
		{
			name:     "two-digit seconds no fraction",
			body:     "[01:07]late",
			wantCues: []string{"67|late"},
		},
		{
			name:     "single-digit seconds dropped (malformed, matches petitlyrics)",
			body:     "[0:5]bad\n[00:03.00]good",
			wantCues: []string{"3|good"},
		},
		{
			name:     "whitespace-separated stacked stamps still expand",
			body:     "[00:12.00] [00:45.00]Chorus",
			wantCues: []string{"12|Chorus", "45|Chorus"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := ParseBody(tt.body)
			if len(doc.Cues) != len(tt.wantCues) {
				t.Fatalf("cue count: want %d, got %d (%+v)", len(tt.wantCues), len(doc.Cues), doc.Cues)
			}
			for i, want := range tt.wantCues {
				got := fmtCue(doc.Cues[i])
				if got != want {
					t.Errorf("cue %d: want %q, got %q", i, want, got)
				}
			}
			if len(doc.Tags) != tt.wantTags {
				t.Errorf("tag count: want %d, got %d", tt.wantTags, len(doc.Tags))
			}
		})
	}
}

func fmtCue(c models.Lines) string {
	return strconv.FormatFloat(c.Time.Total, 'f', -1, 64) + "|" + c.Text
}

func TestParseBody_ClassifiesTags(t *testing.T) {
	body := "[ar:Some Artist]\n[ti:A Title]\n[length:03:21]\n[00:10.00]First line"
	doc := ParseBody(body)

	if len(doc.Cues) != 1 {
		t.Fatalf("want 1 cue, got %d", len(doc.Cues))
	}
	if doc.Cues[0].Text != "First line" {
		t.Errorf("cue text: want %q, got %q", "First line", doc.Cues[0].Text)
	}
	if len(doc.Tags) != 3 {
		t.Fatalf("want 3 tags, got %d: %+v", len(doc.Tags), doc.Tags)
	}
	want := []Tag{
		{Key: "ar", Value: "Some Artist", Raw: "[ar:Some Artist]"},
		{Key: "ti", Value: "A Title", Raw: "[ti:A Title]"},
		{Key: "length", Value: "03:21", Raw: "[length:03:21]"},
	}
	for i, w := range want {
		if doc.Tags[i] != w {
			t.Errorf("tag %d: want %+v, got %+v", i, w, doc.Tags[i])
		}
	}
}

func TestParseBody_SortsAscendingStable(t *testing.T) {
	// Out-of-order intra-line stack, plus a second line that interleaves in time.
	doc := ParseBody("[02:14.00][00:45.00]Chorus\n[01:00.00]Verse")

	if len(doc.Cues) != 3 {
		t.Fatalf("want 3 cues, got %d", len(doc.Cues))
	}
	wantTotal := []float64{45, 60, 134}
	wantText := []string{"Chorus", "Verse", "Chorus"}
	for i, c := range doc.Cues {
		if c.Time.Total != wantTotal[i] {
			t.Errorf("cue %d total: want %v, got %v", i, wantTotal[i], c.Time.Total)
		}
		if c.Text != wantText[i] {
			t.Errorf("cue %d text: want %q, got %q", i, wantText[i], c.Text)
		}
	}
}

func TestParseBody_StackedTimestamps(t *testing.T) {
	doc := ParseBody("[00:30.00][01:05.00][02:10.00]Chorus line")

	if len(doc.Cues) != 3 {
		t.Fatalf("want 3 cues, got %d", len(doc.Cues))
	}
	wantSecs := []float64{30, 65, 130}
	for i, c := range doc.Cues {
		if c.Text != "Chorus line" {
			t.Errorf("cue %d text: want %q, got %q", i, "Chorus line", c.Text)
		}
		if c.Time.Total != wantSecs[i] {
			t.Errorf("cue %d total: want %v, got %v", i, wantSecs[i], c.Time.Total)
		}
	}
}

func TestParseBody_SingleTimestamp(t *testing.T) {
	doc := ParseBody("[00:15.05]Hello world")

	if len(doc.Cues) != 1 {
		t.Fatalf("want 1 cue, got %d", len(doc.Cues))
	}
	c := doc.Cues[0]
	if c.Text != "Hello world" {
		t.Errorf("text: want %q, got %q", "Hello world", c.Text)
	}
	if c.Time.Minutes != 0 || c.Time.Seconds != 15 || c.Time.Hundredths != 5 {
		t.Errorf("time: want 00:15.05, got %02d:%02d.%02d", c.Time.Minutes, c.Time.Seconds, c.Time.Hundredths)
	}
	if c.Time.Total != 15.05 {
		t.Errorf("total: want 15.05, got %v", c.Time.Total)
	}
}

// TestParseBody_PreservesA2WordMarkers is issue #480's stated acceptance
// criterion: an Enhanced-LRC (A2) round trip must prove the inline <mm:ss.xx>
// word markers survive expansion.
//
// Why this matters independently of any media player: canticle reads its own
// sidecars back off disk through ParseBody (the revalidate path, #442). If the
// A2 writer emits word markers and this normalizer then mangles them on read,
// that is a defect regardless of whether any player renders them.
//
// The mechanism the test pins is tsRe: it is anchored (^) and requires square
// brackets, so an angle-bracket marker cannot match on either count, and
// expandLine stops consuming once non-timestamp text begins. The word markup is
// therefore carried as literal cue TEXT.
//
// The body below is a MINIMIZED A2 document, not a copy of the sample
// generator's output: it carries the same cue-line shape (a leading [mm:ss.xx]
// followed by inline <mm:ss.xx> word markers) but omits the [al]/[by] tags, the
// third cue, and the trailing spaces that internal/lyrics/a2sample_test.go
// emits. An earlier version of this comment claimed the two were byte-identical.
// They are not, and the claim would have sent a reader trying to keep them in
// sync.
//
// They are deliberately NOT shared. The parser behavior pinned here is
// permanent, while that generator is throwaway scaffolding marked for deletion,
// and coupling this test to it would tie a lasting guarantee to code with a
// stated expiry. What matters is the SHAPE, and both carry it.
//
// All content is synthesized placeholder words.
func TestParseBody_PreservesA2WordMarkers(t *testing.T) {
	const a2Body = "[ar:Canticle Test Signal]\n" +
		"[ti:Enhanced LRC Probe]\n" +
		"\n" +
		"[00:01.00]<00:01.00>one <00:01.80>two <00:02.60>three <00:03.40>four\n" +
		"[00:05.00]<00:05.00>five <00:05.80>six <00:06.60>seven <00:07.40>eight\n"

	doc := ParseBody(a2Body)

	if len(doc.Cues) != 2 {
		t.Fatalf("want 2 cues (one per A2 line, NOT one per word marker), got %d: %+v",
			len(doc.Cues), doc.Cues)
	}

	// The leading line-level stamp is consumed as the cue's timestamp.
	if doc.Cues[0].Time.Total != 1 {
		t.Errorf("cue 0 timestamp: want 1s, got %v", doc.Cues[0].Time.Total)
	}
	if doc.Cues[1].Time.Total != 5 {
		t.Errorf("cue 1 timestamp: want 5s, got %v", doc.Cues[1].Time.Total)
	}

	// Every word marker survives verbatim in the cue text.
	wantCue0 := "<00:01.00>one <00:01.80>two <00:02.60>three <00:03.40>four"
	if doc.Cues[0].Text != wantCue0 {
		t.Errorf("A2 word markers did not survive the round trip:\n want %q\n got  %q",
			wantCue0, doc.Cues[0].Text)
	}
	wantCue1 := "<00:05.00>five <00:05.80>six <00:06.60>seven <00:07.40>eight"
	if doc.Cues[1].Text != wantCue1 {
		t.Errorf("A2 word markers did not survive the round trip:\n want %q\n got  %q",
			wantCue1, doc.Cues[1].Text)
	}

	// Header tags are still classified as tags, not swallowed by the A2 lines.
	if len(doc.Tags) != 2 {
		t.Errorf("want 2 header tags alongside A2 cues, got %d: %+v", len(doc.Tags), doc.Tags)
	}
}

// TestExpand_IdempotentOnA2Cues pins the second half of #480's criterion: the
// backfill path (Expand over already-parsed cues) must not split an A2 cue.
//
// This is the failure that would actually bite. Expand splits a cue whose TEXT
// carries an embedded timestamp, and a naive stamp matcher would see twelve
// "timestamps" in one A2 line and shatter it into twelve cues, each carrying the
// wrong text. The petitlyrics client already guards against a split shifting its
// word-timing indices (client.go:326); this proves the split does not happen.
func TestExpand_IdempotentOnA2Cues(t *testing.T) {
	in := models.Synced{Lines: []models.Lines{
		{Text: "<00:01.00>one <00:01.80>two <00:02.60>three <00:03.40>four",
			Time: models.Time{Total: 1, Seconds: 1}},
		{Text: "<00:05.00>five <00:05.80>six <00:06.60>seven <00:07.40>eight",
			Time: models.Time{Total: 5, Seconds: 5}},
	}}

	out := Expand(in)

	if len(out.Lines) != len(in.Lines) {
		t.Fatalf("Expand split an A2 cue: want %d cues, got %d. A split shifts every "+
			"later word-timing index, which is exactly what the petitlyrics client's "+
			"length guard exists to detect.\n%+v", len(in.Lines), len(out.Lines), out.Lines)
	}
	for i := range out.Lines {
		if out.Lines[i].Text != in.Lines[i].Text {
			t.Errorf("cue %d text mutated:\n want %q\n got  %q", i, in.Lines[i].Text, out.Lines[i].Text)
		}
		if out.Lines[i].Time.Total != in.Lines[i].Time.Total {
			t.Errorf("cue %d timestamp mutated: want %v, got %v",
				i, in.Lines[i].Time.Total, out.Lines[i].Time.Total)
		}
	}
}
