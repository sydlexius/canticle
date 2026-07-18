package lrcnormalize

import (
	"strconv"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

func TestParseBody_TrimsCueText(t *testing.T) {
	// Cue text is trimmed, matching petitlyrics.parseLRC (strings.TrimSpace).
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
