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

// mkTime builds a models.Time from the fields that matter for pairing and
// rendering; Total is left zero since writeSyncedLRC's pairing is defined
// over the rendered mm:ss.xx stamp (models.Time.Stamp()), never Total. Every
// case below stays under a minute, so Minutes is fixed at 0.
func mkTime(seconds, hundredths int) models.Time {
	return models.Time{Seconds: seconds, Hundredths: hundredths}
}

// mkLine is a terser models.Lines constructor for the pairing tests below.
func mkLine(text string, seconds, hundredths int) models.Lines {
	return models.Lines{Text: text, Time: mkTime(seconds, hundredths)}
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

// TestWriteLRC_BilingualFlagOnNoTranslation verifies that with the flag on but
// no translation track, output is identical to the original-only default. This
// also stands in for the "empty translation track" case: interleave is false
// whenever TranslationSubtitles.Lines is empty, so the pairing logic is never
// reached and output must be byte-identical to the original-only path.
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

// TestWriteLRC_BilingualInterleavedByTimestamp is the equal-counts regression
// guard: original and translation cues share timestamps one-to-one, so both
// index pairing and timestamp pairing agree on the output. This pins the
// unchanged-shape case #489's fix must not disturb.
func TestWriteLRC_BilingualInterleavedByTimestamp(t *testing.T) {
	w := NewLRCWriter()
	w.SetBilingual(true)
	dir := t.TempDir()
	song := models.Song{
		Track: models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles: models.Synced{Lines: []models.Lines{
			mkLine("original one", 12, 50),
			mkLine("original two", 15, 0),
		}},
		TranslationSubtitles: models.Synced{Lines: []models.Lines{
			mkLine("translation one", 12, 50),
			mkLine("translation two", 15, 0),
		}},
	}
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	out := readWritten(t, dir)
	want := "[00:12.50]original one\n[00:12.50]translation one\n[00:15.00]original two\n[00:15.00]translation two\n"
	if !strings.Contains(out, want) {
		t.Errorf("interleaved body mismatch.\nwant substring:\n%q\ngot:\n%q", want, out)
	}
}

// TestWriteLRC_BilingualFewerTranslationCues verifies that when the
// translation track has FEWER cues than the original, each translation cue
// pairs with the ORIGINAL cue sharing its timestamp -- not with the original
// cue at the same slice position. The translation's only cue is stamped to
// match the original's SECOND line, which index pairing would wrongly attach
// to the first.
func TestWriteLRC_BilingualFewerTranslationCues(t *testing.T) {
	w := NewLRCWriter()
	w.SetBilingual(true)
	dir := t.TempDir()
	song := models.Song{
		Track: models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles: models.Synced{Lines: []models.Lines{
			mkLine("original one", 12, 50),
			mkLine("original two", 15, 0),
		}},
		TranslationSubtitles: models.Synced{Lines: []models.Lines{
			mkLine("translation two", 15, 0),
		}},
	}
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	out := readWritten(t, dir)
	want := "[00:12.50]original one\n[00:15.00]original two\n[00:15.00]translation two\n"
	if !strings.Contains(out, want) {
		t.Errorf("fewer-translation-cues body mismatch.\nwant substring:\n%q\ngot:\n%q", want, out)
	}
	if strings.Contains(out, "[00:12.50]original one\n[00:12.50]translation two") {
		t.Errorf("translation must not be misattributed to the first original cue by position:\n%s", out)
	}
}

// TestWriteLRC_BilingualMoreTranslationCues verifies that a surplus
// translation cue whose timestamp matches no original cue is dropped, while a
// translation cue at a timestamp that DOES match an original cue pairs
// correctly regardless of its position in the slice.
func TestWriteLRC_BilingualMoreTranslationCues(t *testing.T) {
	w := NewLRCWriter()
	w.SetBilingual(true)
	dir := t.TempDir()
	song := models.Song{
		Track: models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles: models.Synced{Lines: []models.Lines{
			mkLine("original one", 12, 50),
			mkLine("original two", 15, 0),
		}},
		TranslationSubtitles: models.Synced{Lines: []models.Lines{
			mkLine("translation one", 12, 50),
			// The orphan sits MID-slice: at the tail, index pairing drops it
			// too (i < len) and the test could not tell the two rules apart.
			mkLine("translation orphan", 13, 0), // no matching original stamp
			mkLine("translation two", 15, 0),
		}},
	}
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	out := readWritten(t, dir)
	want := "[00:12.50]original one\n[00:12.50]translation one\n[00:15.00]original two\n[00:15.00]translation two\n"
	if !strings.Contains(out, want) {
		t.Errorf("more-translation-cues body mismatch.\nwant substring:\n%q\ngot:\n%q", want, out)
	}
	if strings.Contains(out, "orphan") {
		t.Errorf("surplus translation cue with no matching timestamp must be dropped:\n%s", out)
	}
}

// TestWriteLRC_BilingualMissingCueInMiddle reproduces issue #489's own repro
// table: after parse-time stacked-line expansion the original track has a
// cue at 25s with no translation counterpart, sandwiched between cues that DO
// have counterparts. Index pairing shifts every cue after the gap by one;
// timestamp pairing must not.
func TestWriteLRC_BilingualMissingCueInMiddle(t *testing.T) {
	w := NewLRCWriter()
	w.SetBilingual(true)
	dir := t.TempDir()
	song := models.Song{
		Track: models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles: models.Synced{Lines: []models.Lines{
			mkLine("alpha", 10, 0),
			mkLine("chorus", 20, 0),
			mkLine("chorus", 25, 0), // stacked-expansion repeat; no translation counterpart
			mkLine("beta", 30, 0),
		}},
		TranslationSubtitles: models.Synced{Lines: []models.Lines{
			mkLine("alpha-t", 10, 0),
			mkLine("chorus-t", 20, 0),
			mkLine("beta-t", 30, 0),
		}},
	}
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	out := readWritten(t, dir)
	want := "[00:10.00]alpha\n[00:10.00]alpha-t\n" +
		"[00:20.00]chorus\n[00:20.00]chorus-t\n" +
		"[00:25.00]chorus\n" +
		"[00:30.00]beta\n[00:30.00]beta-t\n"
	if !strings.Contains(out, want) {
		t.Errorf("middle-gap body mismatch.\nwant substring:\n%q\ngot:\n%q", want, out)
	}
	if strings.Contains(out, "[00:25.00]chorus\n[00:25.00]") {
		t.Errorf("the unmatched middle cue must be emitted alone, not paired with beta-t:\n%s", out)
	}
}

// TestWriteLRC_BilingualReorderedTranslation proves pairing is keyed on the
// timestamp, never on slice position: the translation track carries the same
// cues as the equal-counts regression case but in reverse order, and the
// output must still pair each original with its own-timestamp translation.
func TestWriteLRC_BilingualReorderedTranslation(t *testing.T) {
	w := NewLRCWriter()
	w.SetBilingual(true)
	dir := t.TempDir()
	song := models.Song{
		Track: models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles: models.Synced{Lines: []models.Lines{
			mkLine("original one", 12, 50),
			mkLine("original two", 15, 0),
		}},
		TranslationSubtitles: models.Synced{Lines: []models.Lines{
			// Reversed relative to the original track.
			mkLine("translation two", 15, 0),
			mkLine("translation one", 12, 50),
		}},
	}
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	out := readWritten(t, dir)
	want := "[00:12.50]original one\n[00:12.50]translation one\n[00:15.00]original two\n[00:15.00]translation two\n"
	if !strings.Contains(out, want) {
		t.Errorf("reordered-translation body mismatch.\nwant substring:\n%q\ngot:\n%q", want, out)
	}
}

// TestWriteLRC_BilingualDuplicateTimestampsFIFO covers two original cues
// sharing one timestamp and two translation cues sharing that same
// timestamp: pairing must be FIFO per stamp (first-with-first,
// second-with-second), never reusing one translation cue for both originals
// and never crossing the pairs.
func TestWriteLRC_BilingualDuplicateTimestampsFIFO(t *testing.T) {
	w := NewLRCWriter()
	w.SetBilingual(true)
	dir := t.TempDir()
	song := models.Song{
		Track: models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles: models.Synced{Lines: []models.Lines{
			mkLine("echo one", 20, 0),
			mkLine("echo two", 20, 0),
		}},
		TranslationSubtitles: models.Synced{Lines: []models.Lines{
			mkLine("echo one-t", 20, 0),
			mkLine("echo two-t", 20, 0),
		}},
	}
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	out := readWritten(t, dir)
	want := "[00:20.00]echo one\n[00:20.00]echo one-t\n[00:20.00]echo two\n[00:20.00]echo two-t\n"
	if !strings.Contains(out, want) {
		t.Errorf("duplicate-timestamp FIFO body mismatch.\nwant substring:\n%q\ngot:\n%q", want, out)
	}
	// Each translation cue must appear exactly once (no reuse across pairs).
	if strings.Count(out, "echo one-t") != 1 || strings.Count(out, "echo two-t") != 1 {
		t.Errorf("each duplicate-timestamp translation cue must be consumed exactly once:\n%s", out)
	}
}

// TestWriteLRC_BilingualCompanionPairsByTimestamp pins that the .elrc companion
// (#986) goes through the same pairing as the .lrc beside it: with diverging cue
// counts both files attach the translation to the cue sharing its stamp, and the
// translation line carries no word markers.
func TestWriteLRC_BilingualCompanionPairsByTimestamp(t *testing.T) {
	w := modeWriter(false, true)
	w.SetBilingual(true)
	dir := t.TempDir()
	song := a2Song()
	song.Subtitles.Lines = append([]models.Lines{mkLine("intro", 0, 50)}, song.Subtitles.Lines...)
	for i := range song.WordTimings {
		song.WordTimings[i].Line = 1
	}
	song.TranslationSubtitles = models.Synced{Lines: []models.Lines{mkLine("alpha-t", 1, 50)}}
	if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	lrc := readFileString(t, filepath.Join(dir, "song.lrc"))
	if want := "[00:00.50]intro\n[00:01.50]alpha beta\n[00:01.50]alpha-t\n"; !strings.Contains(lrc, want) {
		t.Errorf(".lrc pairing mismatch.\nwant substring:\n%q\ngot:\n%q", want, lrc)
	}
	elrc := readFileString(t, filepath.Join(dir, "song.elrc"))
	if want := "[00:00.50]intro\n[00:01.50]<00:01.50>alpha <00:02.00>beta\n[00:01.50]alpha-t\n"; !strings.Contains(elrc, want) {
		t.Errorf(".elrc pairing mismatch.\nwant substring:\n%q\ngot:\n%q", want, elrc)
	}
}
