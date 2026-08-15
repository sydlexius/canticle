package lyrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// TestGenerateA2Sample writes a known-good Enhanced-LRC (A2) file so the
// maintainer can load it into Emby and Symphonium and judge whether they render
// word markers or show them as literal text.
//
// That verdict gates #480: a non-supporting player displays "<mm:ss.xx>" inline,
// which is worse than clean line-sync. Music Assistant is known-not-supporting
// (filed 2026-07-15).
//
// Content is synthesized, never a real lyric.
//
// Run: A2SAMPLE_DIR=/path/to/scratchpad go test -run TestGenerateA2Sample ./internal/lyrics -v
func TestGenerateA2Sample(t *testing.T) {
	dir := os.Getenv("A2SAMPLE_DIR")
	if dir == "" {
		t.Skip("set A2SAMPLE_DIR to generate the A2 player-verification sample")
	}

	song := models.Song{
		Track: models.Track{
			ArtistName: "Canticle Test Signal",
			TrackName:  "Enhanced LRC Probe",
			AlbumName:  "Player Verification",
		},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "one two three four", Time: models.Time{Total: 1.0, Minutes: 0, Seconds: 1, Hundredths: 0}},
			{Text: "five six seven eight", Time: models.Time{Total: 5.0, Minutes: 0, Seconds: 5, Hundredths: 0}},
			{Text: "nine ten eleven twelve", Time: models.Time{Total: 9.0, Minutes: 0, Seconds: 9, Hundredths: 0}},
		}},
		WordTimings: []models.WordTiming{
			{Line: 0, Text: "one", StartMS: 1000, EndMS: 1800},
			{Line: 0, Text: "two", StartMS: 1800, EndMS: 2600},
			{Line: 0, Text: "three", StartMS: 2600, EndMS: 3400},
			{Line: 0, Text: "four", StartMS: 3400, EndMS: 4200},
			{Line: 1, Text: "five", StartMS: 5000, EndMS: 5800},
			{Line: 1, Text: "six", StartMS: 5800, EndMS: 6600},
			{Line: 1, Text: "seven", StartMS: 6600, EndMS: 7400},
			{Line: 1, Text: "eight", StartMS: 7400, EndMS: 8200},
			{Line: 2, Text: "nine", StartMS: 9000, EndMS: 9800},
			{Line: 2, Text: "ten", StartMS: 9800, EndMS: 10600},
			{Line: 2, Text: "eleven", StartMS: 10600, EndMS: 11400},
			{Line: 2, Text: "twelve", StartMS: 11400, EndMS: 12200},
		},
	}

	body := renderA2(song)
	path := filepath.Join(dir, "a2-player-test.lrc")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write A2 sample: %v", err)
	}

	t.Logf("A2 sample written to %s\n\n%s", path, body)
	t.Logf("VERDICT NEEDED: load this in Emby and Symphonium. " +
		"Word markers rendering as karaoke-style highlighting means A2 is supported. " +
		"Literal '<00:01.00>' text on screen means it is NOT, and #480 should stay shelved.")
}

// renderA2 emits Enhanced-LRC: a leading line-level [mm:ss.xx] for backward
// compatibility, then inline <mm:ss.xx> markers before each word.
//
// This is a THROWAWAY renderer for the player-verification sample only. The real
// writer is #480's job and belongs in writer.go with the full timing-guard and
// provenance path; do not promote this.
func renderA2(song models.Song) string {
	byLine := map[int][]models.WordTiming{}
	for _, wt := range song.WordTimings {
		byLine[wt.Line] = append(byLine[wt.Line], wt)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[ar:%s]\n", song.Track.ArtistName)
	fmt.Fprintf(&b, "[ti:%s]\n", song.Track.TrackName)
	fmt.Fprintf(&b, "[al:%s]\n", song.Track.AlbumName)
	b.WriteString("[by:canticle A2 player-support probe]\n\n")

	for i, line := range song.Subtitles.Lines {
		fmt.Fprintf(&b, "[%s]", stampFromMS(int(line.Time.Total*1000)))
		for _, wt := range byLine[i] {
			fmt.Fprintf(&b, "<%s>%s ", stampFromMS(wt.StartMS), wt.Text)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// stampFromMS formats milliseconds as mm:ss.xx.
func stampFromMS(ms int) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d.%02d", ms/60000, (ms/1000)%60, (ms/10)%100)
}

// TestRenderA2Shape asserts the sample renderer emits both marker kinds, since a
// player verdict is meaningless if the file it was judged on was malformed.
func TestRenderA2Shape(t *testing.T) {
	song := models.Song{
		Track: models.Track{ArtistName: "A", TrackName: "T", AlbumName: "AL"},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "alpha beta", Time: models.Time{Total: 1.5}},
		}},
		WordTimings: []models.WordTiming{
			{Line: 0, Text: "alpha", StartMS: 1500, EndMS: 2000},
			{Line: 0, Text: "beta", StartMS: 2000, EndMS: 2500},
		},
	}

	got := renderA2(song)

	if !strings.Contains(got, "[00:01.50]") {
		t.Errorf("missing the line-level cue for backward compatibility:\n%s", got)
	}
	if !strings.Contains(got, "<00:01.50>alpha") {
		t.Errorf("missing the first word marker:\n%s", got)
	}
	if !strings.Contains(got, "<00:02.00>beta") {
		t.Errorf("missing the second word marker:\n%s", got)
	}
}
