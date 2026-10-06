package aligner

import (
	"math"
	"strings"
	"unicode"

	"github.com/sydlexius/canticle/internal/verification"
)

// Warning thresholds for a suggestion (#1008). A measure strictly below its
// threshold warns; a warning never blocks an accept. Constants, not config,
// as timing.Tolerance is: one definition for every caller.
const (
	// WarnSimilarity is the verification.min_similarity default, applied to
	// the sidecar's own transcript against the lines that were sent.
	WarnSimilarity = 0.35
	// WarnMeanConfidence is PROVISIONAL: #1015 published only a right-versus-
	// wrong confidence gap, not absolute levels. Recalibrate before relying on it.
	WarnMeanConfidence = 0.50
	// WarnCoverage is the share of sent lyric tokens the sidecar aligned.
	WarnCoverage = 0.80
)

// Warning codes, in the order Warnings reports them. Machine codes, not prose.
const (
	WarningSimilarity = "similarity"
	WarningConfidence = "confidence"
	WarningCoverage   = "coverage"
	WarningMerged     = "merged"
)

// Quality is what a suggestion's result says about itself. Every ratio is in
// [0, 1]; a measure with nothing to measure is 0, so it warns, except Similarity.
type Quality struct {
	// Similarity is verification.Similarity(transcript, sent lines). It is
	// undefined, 0 and never warned on, when HasTranscript is false.
	Similarity float64
	// HasTranscript is false when the transcript holds no letter or digit, which
	// valid words do not rule out: it is a separate, voice-filtered ASR pass.
	HasTranscript bool
	// MeanConfidence is the mean per-word confidence over the words Suggest
	// used; a NaN or negative confidence counts as 0 and one above 1 as 1.
	MeanConfidence float64
	// Coverage is AlignedTokens / Tokens: the sent lines' tokens as the sidecar
	// splits them, and those an aligned word was matched to.
	Coverage              float64
	Tokens, AlignedTokens int
	// Merged counts adjacent lines with different current starts that the
	// suggestion puts in one hundredth of a second: one stamp on disk, for good.
	Merged int
}

// Warnings returns the codes of the measures below their thresholds, in the order
// similarity, confidence, coverage, then merged if Merged is not 0. Never nil; NaN warns.
func (q Quality) Warnings() []string {
	out := []string{}
	if !q.HasTranscript {
		q.Similarity = WarnSimilarity // undefined: not a warning
	}
	for _, m := range []struct {
		code      string
		v, thresh float64
	}{
		{WarningSimilarity, q.Similarity, WarnSimilarity},
		{WarningConfidence, q.MeanConfidence, WarnMeanConfidence},
		{WarningCoverage, q.Coverage, WarnCoverage},
	} {
		if !(m.v >= m.thresh) { // negated so NaN warns
			out = append(out, m.code)
		}
	}
	if q.Merged != 0 {
		out = append(out, WarningMerged)
	}
	return out
}

// measure computes Quality from the lines sent, the words Suggest used (by
// line) and the token counts its matching produced.
func measure(lines []string, byLine [][]Word, transcript string, tokens, aligned int) Quality {
	q := Quality{Tokens: tokens, AlignedTokens: aligned}
	var sent []string
	sum, words := 0.0, 0
	for i, l := range lines {
		if !isPythonBlank(l) {
			sent = append(sent, l)
		}
		for _, w := range byLine[i] {
			words++
			if c := w.Confidence; !math.IsNaN(c) && c > 0 {
				sum += math.Min(c, 1)
			}
		}
	}
	if q.HasTranscript = strings.IndexFunc(transcript, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) >= 0; q.HasTranscript {
		q.Similarity = verification.Similarity(transcript, strings.Join(sent, " "))
	}
	if words > 0 {
		q.MeanConfidence = sum / float64(words)
	}
	if tokens > 0 {
		q.Coverage = float64(aligned) / float64(tokens)
	}
	return q
}
