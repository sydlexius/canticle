package lyrics

import (
	"bufio"
	"bytes"
	"reflect"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

func TestParseTimedLRC_LineOnly(t *testing.T) {
	got := ParseTimedLRC("[ar:Nobody]\n[00:01.50]first line\n[01:02.03]second line\n")
	if got.HasWords {
		t.Fatal("HasWords = true for a line-only file")
	}
	if len(got.Tags) != 1 || got.Tags[0].Key != "ar" {
		t.Fatalf("Tags = %+v, want one ar tag", got.Tags)
	}
	want := []TimedLine{
		{StartMS: 1500, Text: "first line"},
		{StartMS: 62030, Text: "second line"},
	}
	if !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("Lines = %+v, want %+v", got.Lines, want)
	}
}

func TestParseTimedLRC_A2Line(t *testing.T) {
	got := ParseTimedLRC("[00:10.00]<00:10.00>alpha <00:10.40>beta <00:11.25>gamma\n")
	if !got.HasWords || len(got.Lines) != 1 {
		t.Fatalf("got %+v", got)
	}
	l := got.Lines[0]
	if l.Text != "alpha beta gamma" || l.StartMS != 10000 {
		t.Fatalf("line = %+v", l)
	}
	want := []TimedWord{{10000, "alpha"}, {10400, "beta"}, {11250, "gamma"}}
	if !reflect.DeepEqual(l.Words, want) {
		t.Fatalf("Words = %+v, want %+v", l.Words, want)
	}
}

func TestParseTimedLRC_StackedTimestampsCarryWords(t *testing.T) {
	got := ParseTimedLRC("[00:05.00][00:30.00]<00:05.00>la <00:05.50>la\n")
	if len(got.Lines) != 2 {
		t.Fatalf("Lines = %+v, want 2", got.Lines)
	}
	for i, wantStart := range []int{5000, 30000} {
		l := got.Lines[i]
		if l.StartMS != wantStart || len(l.Words) != 2 || l.Text != "la la" {
			t.Fatalf("line %d = %+v", i, l)
		}
	}
}

func TestParseTimedLRC_MalformedMarkersDoNotPanic(t *testing.T) {
	cases := map[string]string{
		"bad seconds":   "[00:01.00]<00:60.00>word",
		"short frac":    "[00:01.00]<1:2.3>word",
		"unclosed":      "[00:01.00]<00:01.00word",
		"empty marker":  "[00:01.00]<>word",
		"marker only":   "[00:01.00]<00:01.00>",
		"adjacent":      "[00:01.00]<00:01.00><00:02.00>word",
		"overflow mins": "[00:01.00]<99999999999999999999:00.00>word",
		"bare angle":    "[00:01.00]a < b > c",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got := ParseTimedLRC(body)
			if len(got.Lines) != 1 {
				t.Fatalf("Lines = %+v, want 1", got.Lines)
			}
		})
	}
	// An overflowing minute field clamps: the marker is consumed, not leaked.
	ov := ParseTimedLRC(cases["overflow mins"]).Lines[0]
	if ov.Text != "word" || len(ov.Words) != 1 || ov.Words[0].StartMS != maxMarkerMinutes*60000 {
		t.Errorf("overflow line = %+v, want Text=word, one word at %d", ov, maxMarkerMinutes*60000)
	}
	// A malformed marker stays literal text and yields no words.
	l := ParseTimedLRC("[00:01.00]<00:60.00>word").Lines[0]
	if l.Text != "<00:60.00>word" || len(l.Words) != 0 {
		t.Fatalf("malformed marker line = %+v", l)
	}
	// Adjacent markers: the empty first word is dropped, the second kept.
	l = ParseTimedLRC("[00:01.00]<00:01.00><00:02.00>word").Lines[0]
	if !reflect.DeepEqual(l.Words, []TimedWord{{2000, "word"}}) {
		t.Fatalf("adjacent markers Words = %+v", l.Words)
	}
}

func TestParseTimedLRC_Decorative(t *testing.T) {
	got := ParseTimedLRC("[00:01.00]♪\n[00:02.00]<00:02.00>♪\n[00:03.00]real words\n")
	want := []bool{true, true, false}
	if len(got.Lines) != 3 {
		t.Fatalf("Lines = %+v", got.Lines)
	}
	for i, d := range want {
		if got.Lines[i].Decorative != d {
			t.Errorf("line %d Decorative = %v, want %v", i, got.Lines[i].Decorative, d)
		}
	}
}

func TestParseTimedLRC_BOMAndEmpty(t *testing.T) {
	if got := ParseTimedLRC(utf8BOM + "[00:01.00]hi\n"); len(got.Lines) != 1 || got.Lines[0].Text != "hi" {
		t.Fatalf("BOM body = %+v", got)
	}
	if got := ParseTimedLRC(""); len(got.Lines) != 0 || got.HasWords {
		t.Fatalf("empty body = %+v", got)
	}
}

// TestParseTimedLRC_RoundTripsWriter feeds the writer's own .elrc-format output
// back through the parser: the word timings must survive exactly, a fallback
// (uniform-start) line must come back as a plain line, and the plain-lrc render
// of the same song must parse to the same line starts.
func TestParseTimedLRC_RoundTripsWriter(t *testing.T) {
	song := models.Song{
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "we sing along", Time: models.MsToTime(12340)},
			{Text: "all together now", Time: models.MsToTime(75000)},
			{Text: "flat line", Time: models.MsToTime(90000)},
		}},
		WordTimings: []models.WordTiming{
			{Line: 0, Text: "we ", StartMS: 12340},
			{Line: 0, Text: "sing ", StartMS: 12900},
			{Line: 0, Text: "along", StartMS: 13550},
			{Line: 1, Text: "all ", StartMS: 75000},
			{Line: 1, Text: "together ", StartMS: 75480},
			{Line: 1, Text: "now", StartMS: 76110},
			// Uniform starts: the writer refuses markers for this line.
			{Line: 2, Text: "flat ", StartMS: 90000},
			{Line: 2, Text: "line", StartMS: 90000},
		},
	}

	var elrc bytes.Buffer
	bw := bufio.NewWriter(&elrc)
	if err := writeSyncedLRC(song, bw, false, true); err != nil {
		t.Fatalf("writeSyncedLRC: %v", err)
	}
	got := ParseTimedLRC(elrc.String())
	if len(got.Lines) != 3 || !got.HasWords {
		t.Fatalf("parsed = %+v", got)
	}

	byLine := wordTimingsByLine(song)
	for i := 0; i < 2; i++ {
		var want []TimedWord
		for _, w := range byLine[i] {
			want = append(want, TimedWord{StartMS: w.StartMS, Text: stripSpace(w.Text)})
		}
		if !reflect.DeepEqual(got.Lines[i].Words, want) {
			t.Errorf("line %d Words = %+v, want %+v", i, got.Lines[i].Words, want)
		}
		if got.Lines[i].Text != song.Subtitles.Lines[i].Text {
			t.Errorf("line %d Text = %q, want %q", i, got.Lines[i].Text, song.Subtitles.Lines[i].Text)
		}
		if got.Lines[i].StartMS != cueStartMS(song.Subtitles.Lines[i].Time) {
			t.Errorf("line %d StartMS = %d", i, got.Lines[i].StartMS)
		}
	}
	if l := got.Lines[2]; len(l.Words) != 0 || l.Text != "flat line" || l.StartMS != 90000 {
		t.Errorf("fallback line = %+v, want plain line", l)
	}

	var plain bytes.Buffer
	pw := bufio.NewWriter(&plain)
	if err := writeSyncedLRC(song, pw, false, false); err != nil {
		t.Fatalf("writeSyncedLRC plain: %v", err)
	}
	pg := ParseTimedLRC(plain.String())
	if pg.HasWords || len(pg.Lines) != 3 {
		t.Fatalf("plain parse = %+v", pg)
	}
	for i := range pg.Lines {
		if pg.Lines[i].StartMS != got.Lines[i].StartMS || pg.Lines[i].Text != got.Lines[i].Text {
			t.Errorf("plain line %d = %+v differs from enhanced %+v", i, pg.Lines[i], got.Lines[i])
		}
	}
}
