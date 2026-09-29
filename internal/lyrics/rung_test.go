package lyrics

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// Candidates, one per rung the writer can land.
func rungSongs() (instrumental, unsynced, line models.Song) {
	instrumental = models.Song{Track: models.Track{ArtistName: "A", TrackName: "T", Instrumental: 1}}
	unsynced = models.Song{Track: models.Track{ArtistName: "A", TrackName: "T"}, Lyrics: models.Lyrics{LyricsBody: "new words"}}
	line = models.Song{Track: models.Track{ArtistName: "A", TrackName: "T"}, Subtitles: models.Synced{Lines: []models.Lines{
		{Text: "new line", Time: models.Time{Total: 1, Seconds: 1}},
	}}}
	return instrumental, unsynced, line
}

func seed(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRungOnDisk pins the classifier to the artifact itself: no provenance
// header decides a rung, and a .lrc outranks a .txt beside it.
func TestRungOnDisk(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  Rung
	}{
		{"nothing", nil, RungNone},
		{"bare marker", map[string]string{"song.txt": InstrumentalMarker + "\n"}, RungInstrumental},
		{"provider marker with header", map[string]string{"song.txt": "[by:canticle]\n[source:musixmatch]\n" + InstrumentalMarker + "\n"}, RungInstrumental},
		{"unsynced text", map[string]string{"song.txt": "plain words\n"}, RungUnsynced},
		{"lrc without timestamps", map[string]string{"song.lrc": "plain words\n"}, RungUnsynced},
		{"line lrc", map[string]string{"song.lrc": "[00:01.00]line\n"}, RungLine},
		{"one word-marked line is enough", map[string]string{"song.lrc": "[00:01.00]line\n[00:02.00]<00:02.00>word <00:02.50>two\n"}, RungWord},
		{"owned companion makes it word", map[string]string{"song.lrc": "[00:01.00]line\n", "song.elrc": "[by:canticle]\n[00:01.00]<00:01.00>line\n"}, RungWord},
		{"lrc outranks txt", map[string]string{"song.lrc": "[00:01.00]line\n", "song.txt": "plain\n"}, RungLine},
		{"extension-case variant", map[string]string{"song.LRC": "[00:01.00]line\n"}, RungLine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				seed(t, filepath.Join(dir, name), body)
			}
			if got := RungOnDisk(filepath.Join(dir, "song.txt"), sidecar.List(dir)); got != tc.want {
				t.Errorf("RungOnDisk = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRungOnDisk_UnreadableKeepsTheFile: a sidecar the guard cannot judge is
// ranked as high as its extension allows, so doubt never licenses a delete.
func TestRungOnDisk_UnreadableKeepsTheFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	dir := t.TempDir()
	lrc := filepath.Join(dir, "song.lrc")
	seed(t, lrc, "[00:01.00]line\n")
	if err := os.Chmod(lrc, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lrc, 0o600) })
	if got := RungOnDisk(lrc, sidecar.List(dir)); got != RungWord {
		t.Errorf("RungOnDisk(unreadable .lrc) = %d, want RungWord", got)
	}
}

// TestWriteLRC_NoDowngrade is the #553 data-loss path: a lower-rung candidate
// must leave every file on disk untouched (no overwrite, no opposite removal,
// no companion removal) and report ErrKeptBetter so callers do not record it.
func TestWriteLRC_NoDowngrade(t *testing.T) {
	instrumental, unsynced, line := rungSongs()
	wordLRC := "[00:01.00]<00:01.00>alpha <00:02.00>beta\n"
	cases := []struct {
		name     string
		seedName string
		seedBody string
		song     models.Song
	}{
		{"unsynced over line lrc", "song.lrc", "[00:01.00]settled\n", unsynced},
		{"instrumental over line lrc", "song.lrc", "[00:01.00]settled\n", instrumental},
		{"instrumental over unsynced txt", "song.txt", "settled words\n", instrumental},
		{"line over word lrc", "song.lrc", wordLRC, line},
		// Qualifying words the writer cannot land (word sync off) write a line .lrc.
		{"unlandable words over word lrc", "song.lrc", wordLRC, a2Song()},
		{"unsynced over case-variant lrc", "song.LRC", "[00:01.00]settled\n", unsynced},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			settled := filepath.Join(dir, tc.seedName)
			seed(t, settled, tc.seedBody)
			err := NewLRCWriter().WriteLRC(tc.song, "song.lrc", dir)
			if !errors.Is(err, ErrKeptBetter) {
				t.Fatalf("WriteLRC err = %v, want ErrKeptBetter", err)
			}
			if got := readFileString(t, settled); got != tc.seedBody {
				t.Errorf("settled sidecar changed: %q", got)
			}
			if names := dirFiles(t, dir); len(names) != 1 {
				t.Errorf("dir = %v, want only the settled sidecar", names)
			}
		})
	}
}

// TestWriteLRC_NoDowngrade_KeepsCompanion: a line-only rewrite of a .lrc whose
// word timing lives in an owned companion would remove the companion; the
// on-disk rung is word, so the write is refused and both files survive.
func TestWriteLRC_NoDowngrade_KeepsCompanion(t *testing.T) {
	_, _, line := rungSongs()
	dir := t.TempDir()
	lrc := filepath.Join(dir, "song.lrc")
	seed(t, lrc, "[00:01.00]settled\n")
	companion := seedCompanion(t, dir)
	if err := modeWriter(false, true).WriteLRC(line, "song.lrc", dir); !errors.Is(err, ErrKeptBetter) {
		t.Fatalf("WriteLRC err = %v, want ErrKeptBetter", err)
	}
	mustExist(t, companion)
	if got := readFileString(t, lrc); got != "[00:01.00]settled\n" {
		t.Errorf(".lrc changed: %q", got)
	}
}

// TestWriteLRC_NoDowngrade_AllowsEqualOrHigher: the guard only refuses a
// strictly lower rung. Same-rung refreshes and upgrades write as before, and a
// forced (--update) write may go down.
func TestWriteLRC_NoDowngrade_AllowsEqualOrHigher(t *testing.T) {
	instrumental, unsynced, line := rungSongs()
	cases := []struct {
		name, seedName, seedBody string
		song                     models.Song
		force                    bool
		want, gone               string
	}{
		{"line over txt upgrades", "song.txt", "old words\n", line, false, "song.lrc", "song.txt"},
		{"unsynced over marker upgrades", "song.txt", InstrumentalMarker + "\n", unsynced, false, "song.txt", ""},
		{"line over line refreshes", "song.lrc", "[00:01.00]old\n", line, false, "song.lrc", ""},
		{"marker over marker refreshes", "song.txt", InstrumentalMarker + "\n", instrumental, false, "song.txt", ""},
		{"forced unsynced over lrc", "song.lrc", "[00:01.00]old\n", unsynced, true, "song.txt", "song.lrc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seed(t, filepath.Join(dir, tc.seedName), tc.seedBody)
			w := NewLRCWriter()
			w.SetForceOverwrite(tc.force)
			if err := w.WriteLRC(tc.song, "song.lrc", dir); err != nil {
				t.Fatalf("WriteLRC: %v", err)
			}
			if got := readFileString(t, filepath.Join(dir, tc.want)); got == tc.seedBody {
				t.Errorf("%s was not rewritten", tc.want)
			}
			if tc.gone != "" {
				mustNotExist(t, filepath.Join(dir, tc.gone))
			}
		})
	}
}
