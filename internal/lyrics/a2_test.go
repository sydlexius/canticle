package lyrics

import (
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
