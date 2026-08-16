package lyrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// wt is a word timing within a single cue. Line is always 0 here: a2Words takes
// ONE line's timings and never reads the field -- grouping by line is the
// caller's job -- so varying it would test nothing.
func wt(text string, startMS, endMS int) models.WordTiming {
	return models.WordTiming{Text: text, StartMS: startMS, EndMS: endMS}
}

// TestA2Words_RendersInlineMarkers is the core of #480's writer: each word gets
// an inline <mm:ss.xx> marker before it, so a player can highlight per word.
func TestA2Words_RendersInlineMarkers(t *testing.T) {
	got, ok := a2Words("alpha beta", []models.WordTiming{
		wt("alpha ", 1500, 2000),
		wt("beta", 2000, 2500),
	})
	if !ok {
		t.Fatalf("a2Words refused a well-formed line: %q", got)
	}
	if got != "<00:01.50>alpha <00:02.00>beta" {
		t.Errorf("a2Words = %q; want %q", got, "<00:01.50>alpha <00:02.00>beta")
	}
}

// TestA2Words_RefusesWhenWordsDoNotReconstructTheLine is the fidelity guard, and
// it is the reason this cannot render straight from the timings the way the
// throwaway probe renderer did.
//
// decodeWordSync builds each cue from the provider's <linestring>, and only
// falls back to joining the word strings when that is absent. So the words and
// the line text are two INDEPENDENT fields that usually agree and are not
// guaranteed to. Rendering only from the timings would then silently emit
// different words than the plain .lrc for the same track -- turning a display
// feature into data loss, invisibly, and only for the tracks where they diverge.
//
// When they disagree the line falls back to a plain cue: word markers are a
// nice-to-have, the words themselves are not.
func TestA2Words_RefusesWhenWordsDoNotReconstructTheLine(t *testing.T) {
	// The timings cover only part of the cue text.
	if got, ok := a2Words("alpha beta gamma", []models.WordTiming{
		wt("alpha ", 1000, 1500),
		wt("beta", 1500, 2000),
	}); ok {
		t.Errorf("a2Words accepted timings that drop a word: %q", got)
	}
	// The timings carry a word the cue does not.
	if got, ok := a2Words("alpha", []models.WordTiming{
		wt("alpha ", 1000, 1500),
		wt("delta", 1500, 2000),
	}); ok {
		t.Errorf("a2Words accepted timings that add a word: %q", got)
	}
}

// TestA2Words_ToleratesWhitespaceDifferences: the provider's <linestring> and
// its per-word strings routinely differ in spacing alone (the join fallback in
// decodeWordSync uses an empty separator, which suits scripts that do not space
// words). That is a formatting difference, not a content one, so it must NOT
// cost the line its markers -- otherwise the guard above would reject most of
// the corpus it is meant to protect.
func TestA2Words_ToleratesWhitespaceDifferences(t *testing.T) {
	got, ok := a2Words("alpha beta", []models.WordTiming{
		wt("alpha", 1000, 1500),
		wt("beta", 1500, 2000),
	})
	if !ok {
		t.Fatalf("a2Words refused a line differing only in spacing: %q", got)
	}
	if !strings.Contains(got, "<00:01.00>alpha") || !strings.Contains(got, "<00:01.50>beta") {
		t.Errorf("a2Words = %q; want both word markers", got)
	}
}

// TestA2Words_RefusesUniformTimestamps: a line whose words ALL share one
// timestamp carries no per-word information -- every word would highlight
// simultaneously. That is the #673 defect at line scope, and the measured
// population has it: across 54 word-synced tracks the median had 100% of words
// distinctly timed and the WORST had 51%.
//
// Emitting markers there would dress a line-synced cue up as word-synced, which
// is worse than not emitting them: it claims detail the data does not carry.
func TestA2Words_RefusesUniformTimestamps(t *testing.T) {
	if got, ok := a2Words("alpha beta gamma", []models.WordTiming{
		wt("alpha ", 3000, 3000),
		wt("beta ", 3000, 3000),
		wt("gamma", 3000, 3000),
	}); ok {
		t.Errorf("a2Words accepted a line whose words share one timestamp: %q", got)
	}
	// A single-word line is NOT uniform in the harmful sense -- one word has one
	// timestamp by definition, and its marker is honest.
	if _, ok := a2Words("alpha", []models.WordTiming{wt("alpha", 3000, 3200)}); !ok {
		t.Error("a2Words refused a legitimate single-word line")
	}
}

// TestA2Words_EmptyTimingsFallsBack: a cue with no word data at all is the
// ordinary line-synced case and must fall back silently.
func TestA2Words_EmptyTimingsFallsBack(t *testing.T) {
	if got, ok := a2Words("alpha beta", nil); ok {
		t.Errorf("a2Words accepted an empty timing set: %q", got)
	}
}

// TestA2Words_SortsByStartTime: the provider's word order within a line is not
// guaranteed to be chronological (decodeWordSync sorts LINES by their first
// word, never the words inside a line). Markers must still ascend, or a player
// reading them in file order would jump backwards mid-line.
func TestA2Words_SortsByStartTime(t *testing.T) {
	got, ok := a2Words("alpha beta", []models.WordTiming{
		wt("beta", 2000, 2500),
		wt("alpha ", 1000, 1500),
	})
	if !ok {
		t.Fatalf("a2Words refused an out-of-order line: %q", got)
	}
	if got != "<00:01.00>alpha <00:02.00>beta" {
		t.Errorf("a2Words = %q; want the words emitted in time order", got)
	}
}

// TestA2Stamp_ClampsNegative: models.WordTiming documents that producers MUST
// clamp to non-negative but that the type does not enforce it, and explicitly
// tells a consumer not to assume. A negative would format as a nonsense stamp.
func TestA2Stamp_ClampsNegative(t *testing.T) {
	if got := a2Stamp(-500); got != "00:00.00" {
		t.Errorf("a2Stamp(-500) = %q; want 00:00.00", got)
	}
}

// TestA2Stamp_Formats covers the wrap points a naive formatter gets wrong.
func TestA2Stamp_Formats(t *testing.T) {
	for _, tc := range []struct {
		ms   int
		want string
	}{
		{0, "00:00.00"},
		{1500, "00:01.50"},
		{61230, "01:01.23"},
		{600000, "10:00.00"},
	} {
		if got := a2Stamp(tc.ms); got != tc.want {
			t.Errorf("a2Stamp(%d) = %q; want %q", tc.ms, got, tc.want)
		}
	}
}

// TestWriteLRC_WordSyncDisabledByDefault pins the default: without opting in,
// output is byte-identical to the pre-#480 line-synced file. Word markers are a
// display feature whose player support is not universal, so an existing library
// must not change shape because a provider happened to serve richer data.
func TestWriteLRC_WordSyncDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	song := a2Song()

	w := NewLRCWriter()
	if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}

	body := readFileString(t, filepath.Join(dir, "song.lrc"))
	if strings.Contains(body, "<00:") {
		t.Errorf("word markers emitted without opting in:\n%s", body)
	}
	if !strings.Contains(body, "[00:01.50]alpha beta") {
		t.Errorf("plain synced cue missing:\n%s", body)
	}
}

// TestWriteLRC_WordSyncEmitsMarkersWhenEnabled is the opt-in path: the leading
// line-level cue stays for players that ignore markers, and each word gains an
// inline marker.
func TestWriteLRC_WordSyncEmitsMarkersWhenEnabled(t *testing.T) {
	dir := t.TempDir()

	w := NewLRCWriter()
	w.SetWordSync(true)
	if err := w.WriteLRC(a2Song(), "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}

	body := readFileString(t, filepath.Join(dir, "song.lrc"))
	// Backward compatibility: the line-level cue is still the first thing on the
	// line, so a player with no A2 support reads it exactly as before.
	if !strings.Contains(body, "[00:01.50]<00:01.50>alpha <00:02.00>beta") {
		t.Errorf("expected a leading line cue followed by word markers:\n%s", body)
	}
}

// TestWriteLRC_WordSyncFallsBackPerLine covers the partially-timed track, which
// the measured corpus contains: one line reconstructs cleanly and gains markers,
// the other does not and must still write its plain cue with its words intact.
//
// Degrading LINE BY LINE rather than all-or-nothing is the point -- a single bad
// line should not cost the whole file its word sync, nor should it cost that
// line its words.
func TestWriteLRC_WordSyncFallsBackPerLine(t *testing.T) {
	dir := t.TempDir()
	song := models.Song{
		Track: models.Track{ArtistName: "A", TrackName: "T"},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "alpha beta", Time: models.Time{Total: 1.5, Seconds: 1, Hundredths: 50}},
			{Text: "gamma delta", Time: models.Time{Total: 3, Seconds: 3}},
		}},
		AudioDurationSeconds: 240,
		WordTimings: []models.WordTiming{
			{Line: 0, Text: "alpha ", StartMS: 1500, EndMS: 2000},
			{Line: 0, Text: "beta", StartMS: 2000, EndMS: 2500},
			// Line 1's timings do not reconstruct its cue: "gamma delta" vs
			// "gamma epsilon". The line keeps its words and loses only markers.
			{Line: 1, Text: "gamma ", StartMS: 3000, EndMS: 3400},
			{Line: 1, Text: "epsilon", StartMS: 3400, EndMS: 3800},
		},
	}

	w := NewLRCWriter()
	w.SetWordSync(true)
	if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}

	body := readFileString(t, filepath.Join(dir, "song.lrc"))
	if !strings.Contains(body, "[00:01.50]<00:01.50>alpha <00:02.00>beta") {
		t.Errorf("the well-formed line lost its markers:\n%s", body)
	}
	if !strings.Contains(body, "[00:03.00]gamma delta") {
		t.Errorf("the fallback line must keep its own words, unmarked:\n%s", body)
	}
	if strings.Contains(body, "epsilon") {
		t.Errorf("a word absent from the cue leaked into the file:\n%s", body)
	}
}

// a2Song is a two-word, one-line synced song with matching word timings.
func a2Song() models.Song {
	return models.Song{
		Track: models.Track{ArtistName: "A", TrackName: "T"},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "alpha beta", Time: models.Time{Total: 1.5, Seconds: 1, Hundredths: 50}},
		}},
		AudioDurationSeconds: 240,
		WordTimings: []models.WordTiming{
			{Line: 0, Text: "alpha ", StartMS: 1500, EndMS: 2000},
			{Line: 0, Text: "beta", StartMS: 2000, EndMS: 2500},
		},
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // reason: test path from t.TempDir
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestA2Words_RefusesLineBreakingWordText covers real data loss: a word
// containing a newline splits the physical LRC line, and everything after the
// split is silently dropped when the file is parsed back.
//
// Verified against the real parser before fixing -- writing
// "[00:01.00]<00:01.00>a\nb <00:02.00>c" and re-reading yields ONE cue whose
// text is "<00:01.00>a"; the rest is gone.
//
// The fidelity guard cannot catch this: the cue text carries the same newline,
// so the concatenation still matches. It needs its own check, and the precedent
// is sanitizeTagValue, which strips \r and \n for exactly this reason -- a
// crafted value must not break out of the line format it is embedded in.
//
// Refusing (rather than stripping the newline) is deliberate: a word whose text
// contains a line break is not a word canticle can honestly mark up, and the
// plain cue still carries every character.
func TestA2Words_RefusesLineBreakingWordText(t *testing.T) {
	for _, bad := range []string{"a\nb", "a\rb", "a\r\nb"} {
		if got, ok := a2Words(bad+" c", []models.WordTiming{
			wt(bad, 1000, 1500),
			wt(" c", 2000, 2500),
		}); ok {
			t.Errorf("a2Words accepted a word containing a line break (%q): %q", bad, got)
		}
	}
	// A control: ordinary text with no control characters still renders.
	if _, ok := a2Words("alpha beta", []models.WordTiming{
		wt("alpha ", 1000, 1500), wt("beta", 2000, 2500),
	}); !ok {
		t.Error("a2Words refused ordinary text; the line-break guard is too broad")
	}
}

// TestA2Words_RefusesEmptyCue covers the empty-cue substitution bypass.
//
// writeSyncedLRC substitutes a music-note glyph for an empty cue, then calls
// a2Words with the ORIGINAL (empty) text and overwrites the result on success --
// discarding the substitution. Whitespace-only or empty word strings strip to ""
// and therefore "reconstruct" an empty cue, so the fidelity guard passed and the
// line rendered as markers plus whitespace instead of the intended glyph.
func TestA2Words_RefusesEmptyCue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cue   string
		words []models.WordTiming
	}{
		{"empty cue, whitespace words", "", []models.WordTiming{wt(" ", 1000, 1500), wt("  ", 2000, 2500)}},
		{"empty cue, empty words", "", []models.WordTiming{wt("", 1000, 1500), wt("", 2000, 2500)}},
		{"whitespace-only cue", "   ", []models.WordTiming{wt(" ", 1000, 1500), wt("  ", 2000, 2500)}},
	} {
		if got, ok := a2Words(tc.cue, tc.words); ok {
			t.Errorf("%s: a2Words accepted a cue with no words: %q", tc.name, got)
		}
	}
}
