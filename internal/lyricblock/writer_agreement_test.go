package lyricblock_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
)

func cue(sec int, text string) models.Lines {
	return models.Lines{Text: text, Time: models.MsToTime(sec * 1000)}
}

func synced(lines ...models.Lines) models.Synced { return models.Synced{Lines: lines} }

// TestSongFingerprintsAgreeWithRealWriter drives the real LRCWriter in each
// output mode and asserts the fingerprint of the file it wrote is one of the
// song's fingerprints, so a block marked from disk matches a later fetch.
func TestSongFingerprintsAgreeWithRealWriter(t *testing.T) {
	line := synced(cue(1, "Zorp the Blent"), cue(5, "Frabble on"), cue(9, "Zorp the Blent"))
	overrun := synced(cue(1, "Zorp the Blent"), cue(120, "Frabble on")) // past 100s of audio: demoted
	cases := []struct {
		name      string
		song      models.Song
		bilingual bool
		wordSync  bool
		wantNone  bool
	}{
		{name: "line synced", song: models.Song{Subtitles: line}},
		{name: "word timed", wordSync: true, song: models.Song{Subtitles: line, WordTimings: []models.WordTiming{
			{Line: 0, Text: "Zorp ", StartMS: 1000, EndMS: 1400}, {Line: 0, Text: "the Blent", StartMS: 1500, EndMS: 2000}}}},
		{name: "unsynced txt", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "Zorp the Blent\nFrabble on\n"}}},
		{name: "demoted with body", song: models.Song{Subtitles: overrun, AudioDurationSeconds: 100, Lyrics: models.Lyrics{LyricsBody: "Wholly other body\n"}}},
		{name: "demoted from cues", song: models.Song{Subtitles: overrun, AudioDurationSeconds: 100}},
		{name: "bilingual", bilingual: true, song: models.Song{Subtitles: line, TranslationSubtitles: synced(cue(1, "Quix vorl"), cue(5, "Dunsy moor"))}},
		{name: "instrumental with a subtitle", wantNone: true, song: models.Song{Track: models.Track{Instrumental: 1}, Subtitles: line}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			w := lyrics.NewLRCWriter(dir)
			w.SetBilingual(tc.bilingual)
			w.SetWordSync(tc.wordSync)
			if err := w.WriteLRC(tc.song, "song.lrc", dir); err != nil {
				t.Fatalf("WriteLRC: %v", err)
			}
			files, _ := filepath.Glob(filepath.Join(dir, "song.*"))
			if len(files) != 1 {
				t.Fatalf("written files = %v, want exactly one", files)
			}
			body, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			if tc.wordSync && !strings.Contains(string(body), "<00:") {
				t.Fatalf("word-timed file carries no word markers: %q", body)
			}
			disk, fps := lyricblock.Fingerprint(string(body)), lyricblock.SongFingerprints(tc.song)
			if tc.wantNone && (disk != "" || len(fps) != 0) {
				t.Fatalf("instrumental: disk %q, song %v; want none on both sides", disk, fps)
			}
			if !tc.wantNone && (disk == "" || !slices.Contains(fps, disk)) {
				t.Errorf("disk fingerprint %q (file %q) not in song fingerprints %v", disk, body, fps)
			}
		})
	}
}
