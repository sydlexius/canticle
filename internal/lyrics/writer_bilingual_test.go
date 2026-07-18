package lyrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// bilingualTestSong builds a synced song with an original track and (optionally)
// a translation track. Timestamps are chosen so each interleaved pair shares a
// single [mm:ss.cc] marker.
func bilingualTestSong(translation []models.Lines) models.Song {
	return models.Song{
		Track: models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "original one", Time: models.Time{Minutes: 0, Seconds: 12, Hundredths: 50}},
			{Text: "original two", Time: models.Time{Minutes: 0, Seconds: 15, Hundredths: 0}},
		}},
		TranslationSubtitles: models.Synced{Lines: translation},
	}
}

func readWritten(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one output file, found %d: %v", len(entries), entries)
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	return string(b)
}

// TestWriteLRC_OriginalOnlyByDefaultWithTranslation verifies that a translation
// track present in the Song is IGNORED when the bilingual flag is off (the
// default). Original-only output must be byte-stable.
func TestWriteLRC_OriginalOnlyByDefaultWithTranslation(t *testing.T) {
	w := NewLRCWriter()
	dir := t.TempDir()
	song := bilingualTestSong([]models.Lines{
		{Text: "translation one", Time: models.Time{Minutes: 9, Seconds: 9, Hundredths: 9}},
		{Text: "translation two", Time: models.Time{Minutes: 9, Seconds: 9, Hundredths: 9}},
	})
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	out := readWritten(t, dir)
	if strings.Contains(out, "translation") {
		t.Errorf("default output must not contain translation lines:\n%s", out)
	}
	if !strings.Contains(out, "[00:12.50]original one") || !strings.Contains(out, "[00:15.00]original two") {
		t.Errorf("expected original lines present:\n%s", out)
	}
}

// TestWriteLRC_BilingualInterleaved verifies that when the flag is set AND a
// translation track is present, each original line is immediately followed by
// the translation line at the ORIGINAL line's timestamp.
func TestWriteLRC_BilingualInterleaved(t *testing.T) {
	w := NewLRCWriter()
	w.SetBilingual(true)
	dir := t.TempDir()
	song := bilingualTestSong([]models.Lines{
		// Deliberately give the translation its own (different) timestamps to
		// prove the merge uses the ORIGINAL line's timestamp, not the translation's.
		{Text: "translation one", Time: models.Time{Minutes: 5, Seconds: 5, Hundredths: 5}},
		{Text: "translation two", Time: models.Time{Minutes: 6, Seconds: 6, Hundredths: 6}},
	})
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	out := readWritten(t, dir)
	want := "[00:12.50]original one\n[00:12.50]translation one\n[00:15.00]original two\n[00:15.00]translation two\n"
	if !strings.Contains(out, want) {
		t.Errorf("interleaved body mismatch.\nwant substring:\n%q\ngot:\n%q", want, out)
	}
}

// TestWriteLRC_BilingualFlagOnNoTranslation verifies that with the flag on but
// no translation track, output is identical to the original-only default.
func TestWriteLRC_BilingualFlagOnNoTranslation(t *testing.T) {
	wOff := NewLRCWriter()
	wOn := NewLRCWriter()
	wOn.SetBilingual(true)
	dirOff := t.TempDir()
	dirOn := t.TempDir()
	song := bilingualTestSong(nil)
	if err := wOff.WriteLRC(song, "", dirOff); err != nil {
		t.Fatalf("WriteLRC off: %v", err)
	}
	if err := wOn.WriteLRC(song, "", dirOn); err != nil {
		t.Fatalf("WriteLRC on: %v", err)
	}
	if readWritten(t, dirOff) != readWritten(t, dirOn) {
		t.Errorf("flag-on with no translation must match original-only output")
	}
}

// TestWriteLRC_BilingualMismatchedLineCounts verifies graceful handling when the
// translation track is shorter or longer than the original: no panic, original
// lines without a translation counterpart are emitted alone, and surplus
// translation lines are dropped.
func TestWriteLRC_BilingualMismatchedLineCounts(t *testing.T) {
	// Translation shorter than original: only the first original gets a pair.
	w := NewLRCWriter()
	w.SetBilingual(true)
	dir := t.TempDir()
	song := bilingualTestSong([]models.Lines{
		{Text: "translation one", Time: models.Time{}},
	})
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC short: %v", err)
	}
	out := readWritten(t, dir)
	want := "[00:12.50]original one\n[00:12.50]translation one\n[00:15.00]original two\n"
	if !strings.Contains(out, want) {
		t.Errorf("short-translation body mismatch.\nwant substring:\n%q\ngot:\n%q", want, out)
	}
	if strings.Count(out, "translation") != 1 {
		t.Errorf("expected exactly one translation line:\n%s", out)
	}

	// Translation longer than original: surplus translation lines are dropped.
	w2 := NewLRCWriter()
	w2.SetBilingual(true)
	dir2 := t.TempDir()
	song2 := bilingualTestSong([]models.Lines{
		{Text: "translation one", Time: models.Time{}},
		{Text: "translation two", Time: models.Time{}},
		{Text: "translation three", Time: models.Time{}},
	})
	if err := w2.WriteLRC(song2, "", dir2); err != nil {
		t.Fatalf("WriteLRC long: %v", err)
	}
	out2 := readWritten(t, dir2)
	if strings.Contains(out2, "translation three") {
		t.Errorf("surplus translation line must be dropped:\n%s", out2)
	}
	if strings.Count(out2, "translation") != 2 {
		t.Errorf("expected exactly two translation lines:\n%s", out2)
	}
}
