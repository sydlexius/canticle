package lyrics

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/sydlexius/canticle/internal/models"
)

// a2Words renders one cue's words as Enhanced-LRC (A2) inline markers:
// `<mm:ss.xx>word` per word, concatenated. It reports false when the line must
// fall back to a plain line-level cue instead.
//
// It is deliberately CONSERVATIVE, and every refusal below is a case where
// emitting markers would be worse than not having them:
//
//   - The words must reconstruct the cue text (whitespace aside). decodeWordSync
//     builds each cue from the provider's <linestring>, falling back to joining
//     the word strings only when that is absent -- so the cue text and the word
//     strings are two INDEPENDENT fields that usually agree and are not
//     guaranteed to. Rendering straight from the timings, as the throwaway probe
//     renderer did, would silently emit different words than the plain .lrc for
//     exactly the tracks where they diverge. That is data loss wearing a feature's
//     clothes; the words matter more than the markers.
//   - The words must not all share one timestamp. That carries no per-word
//     information -- every word highlights at once -- and is the #673 defect at
//     line scope. The measured corpus has it: across 54 word-synced tracks the
//     median had 100% of words distinctly timed and the worst 51%. Emitting
//     markers there would claim detail the data does not carry.
//
// Falling back costs only the per-word markers; the line still writes its normal
// cue, so a partially-timed track degrades line by line rather than all at once.
func a2Words(lineText string, timings []models.WordTiming) (string, bool) {
	if len(timings) == 0 {
		return "", false
	}

	// Copy before sorting: the caller's slice is shared across lines and its
	// order is meaningful to other consumers.
	ordered := make([]models.WordTiming, len(timings))
	copy(ordered, timings)
	// Stable, so words genuinely sharing a start keep their provider order rather
	// than being permuted into a different reading.
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].StartMS < ordered[j].StartMS })

	if !wordsReconstructLine(lineText, ordered) {
		return "", false
	}
	if uniformStarts(ordered) {
		return "", false
	}

	var b strings.Builder
	for _, t := range ordered {
		fmt.Fprintf(&b, "<%s>%s", a2Stamp(t.StartMS), t.Text)
	}
	return b.String(), true
}

// wordsReconstructLine reports whether the concatenated word strings are the
// same content as the cue text, ignoring whitespace differences.
//
// Whitespace is ignored deliberately rather than leniently: decodeWordSync's
// join fallback uses an EMPTY separator (suiting scripts that do not space
// words), while a <linestring> for a spaced language carries the spaces. A
// byte-exact comparison would therefore reject most of the corpus this guard
// exists to protect, leaving it technically correct and practically useless.
func wordsReconstructLine(lineText string, timings []models.WordTiming) bool {
	var joined strings.Builder
	for _, t := range timings {
		joined.WriteString(t.Text)
	}
	return stripSpace(joined.String()) == stripSpace(lineText)
}

// stripSpace removes every Unicode space so two spellings of the same content
// compare equal.
func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// uniformStarts reports whether every word starts at the same instant, which
// makes the markers cosmetic rather than informative. A single-word line is
// never uniform in the harmful sense: one word has one start by definition, and
// its marker is honest.
func uniformStarts(timings []models.WordTiming) bool {
	if len(timings) < 2 {
		return false
	}
	first := timings[0].StartMS
	for _, t := range timings[1:] {
		if t.StartMS != first {
			return false
		}
	}
	return true
}

// a2Stamp formats milliseconds as the mm:ss.xx an A2 marker carries.
//
// Negative input clamps to zero: models.WordTiming documents that producers MUST
// clamp but that the type does not enforce it, and tells consumers not to
// assume. Minutes deliberately do NOT wrap at 60 -- a 70-minute track renders
// 70:00.00, which is what LRC readers expect, rather than restarting at 10:00.
func a2Stamp(ms int) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d.%02d", ms/60000, (ms/1000)%60, (ms/10)%100)
}
