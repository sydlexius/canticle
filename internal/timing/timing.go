// Package timing centralizes lyric-vs-audio timing classification so the
// accept-time guard, the revalidate CLI, and the serve sweep stay in sync
// rather than drifting apart with three near-identical predicates. It is pure
// logic: no I/O, and it depends only on internal/models and the standard
// library. See #438. Nothing imports it yet: it lands ahead of its consumers so
// the predicate exists once before #439/#442/#443 are built against it.
package timing

import (
	"math"
	"strings"

	"github.com/sydlexius/canticle/internal/models"
)

// TimingOutcome classifies a synced lyric against the audio duration. It is
// string-backed so it stays readable in logs and metrics.
type TimingOutcome string

const (
	// Ok means the lyric ends within the audio (allowing Tolerance), or there
	// is no timing evidence of an overrun.
	Ok TimingOutcome = "ok"
	// MisSynced means the lyric runs past the audio by more than Tolerance but
	// stays under CategoricalRatio. The words are content-correct (see
	// Investigation-0 on #438), so callers demote and keep them.
	MisSynced TimingOutcome = "mis_synced"
	// Categorical means the lyric overran by more than Tolerance AND its last
	// line sits at or beyond CategoricalRatio times the duration, i.e. it is
	// almost certainly timed to a different, longer recording. Callers
	// quarantine and drop the suspect words.
	//
	// Tolerance is checked first, so on very short audio a large ratio can still
	// be Ok: at a 3s duration a 5s last cue is ratio 1.67 but only a 2s overrun.
	// That ordering is deliberate -- the categorical action DISCARDS words, and
	// an absolute overrun within rounding noise is not evidence of a different
	// recording however large the ratio looks.
	Categorical TimingOutcome = "categorical"
	// UnknownDuration means the audio duration was not known, so no judgment
	// is possible. Always fail open: never reject on this.
	UnknownDuration TimingOutcome = "unknown_duration"
)

const (
	// Tolerance is the overrun, in seconds, absorbed before a lyric is called
	// MisSynced. Set at 2s to clear the integer-second rounding spike (the
	// 0-1s band) with margin, so nothing is flagged on rounding alone.
	Tolerance = 2.0
	// CategoricalRatio is the max_ts/duration ratio at which a lyric is treated
	// as timed to a different recording rather than merely drifting. A last
	// line at 1.5x the duration runs 50% past the end.
	CategoricalRatio = 1.5
)

// Magnitude carries the metrics companions to a TimingOutcome. It is the zero
// value whenever no comparison was made -- both the UnknownDuration case and the
// Ok-with-no-timing-evidence case (empty or all-decorative lines) -- and
// Measured is the explicit signal for that. Callers persisting these numbers
// MUST gate on Measured: a real measurement of 0 is a lyric ending exactly at
// the audio end, which is a distinct fact from "nothing was compared", and the
// two outcomes both surface as OverrunSeconds 0 without this flag.
type Magnitude struct {
	// OverrunSeconds is correctedMax - duration. Negative for a lyric that ends
	// before the audio does (e.g. a long instrumental outro). Meaningful only
	// when Measured.
	OverrunSeconds float64
	// Ratio is correctedMax / duration. Meaningful only when Measured.
	Ratio float64
	// Measured reports whether a real comparison against a known duration and a
	// text-bearing lyric happened. False for both no-evidence cases, where the
	// other two fields are zero and carry no information.
	Measured bool
}

// Evaluate classifies song's synced lines against durationSeconds, returning the
// outcome and its magnitude. Ground-truth duration is the audio file, not
// provider metadata.
//
// The comparison uses the CORRECTED max: the largest timestamp among lines that
// carry actual lyric text, with decorative, whitespace-only, and [tag:...] lines
// filtered out wherever they appear. Keying on the raw last timestamp instead
// demotes perfectly-synced lyrics that merely park a music-note glyph past the
// end -- 31% of the flagged tail on the 28.7k corpus. Tolerance and
// CategoricalRatio are calibrated against this corrected max and are not valid
// against the raw one.
func Evaluate(song models.Song, durationSeconds int) (TimingOutcome, Magnitude) {
	if durationSeconds <= 0 {
		return UnknownDuration, Magnitude{}
	}

	maxTS, ok := correctedMaxSeconds(song.Subtitles.Lines)
	if !ok {
		// Empty subtitles, nothing but decorative lines, or no cue with a usable
		// (finite) timestamp: duration is known, but there is no timing evidence
		// to judge. Fail open. Note this is Ok rather than UnknownDuration --
		// the duration is known; it is the lyric side that carries nothing.
		return Ok, Magnitude{}
	}
	duration := float64(durationSeconds)
	mag := Magnitude{OverrunSeconds: maxTS - duration, Ratio: maxTS / duration, Measured: true}

	switch {
	case mag.OverrunSeconds <= Tolerance:
		return Ok, mag
	case mag.Ratio >= CategoricalRatio:
		return Categorical, mag
	default:
		return MisSynced, mag
	}
}

// correctedMaxSeconds returns the largest timestamp among text-bearing lines,
// and whether any such line exists.
//
// Decorative lines are FILTERED wherever they appear, not trimmed off the tail.
// A positional trim would be incoherent here: cue order is not guaranteed (this
// accepts any models.Song, including musixmatch's directly-unmarshalled
// subtitles, which never pass through lrcnormalize's sort), so "trailing" is not
// a well-defined position and a decorative marker listed before a shorter cue
// would still set the max -- reintroducing exactly the false demotion this
// package exists to prevent.
// Non-finite timestamps are skipped PER LINE rather than checked once on the
// result. Total is json.Unmarshal'd straight from the provider payload, so a
// broken or hostile response can put a NaN or Inf on any cue -- and NaN is
// sticky in a running maximum, since every `s > NaN` comparison is false. A
// single NaN on an early cue would otherwise swallow a genuinely categorical
// overrun on a later one, making the verdict depend on cue order.
func correctedMaxSeconds(lines []models.Lines) (float64, bool) {
	maxTS, found := 0.0, false
	for _, l := range lines {
		if isDecorative(l.Text) {
			continue
		}
		s := seconds(l.Time)
		if math.IsNaN(s) || math.IsInf(s, 0) {
			continue
		}
		if !found || s > maxTS {
			maxTS, found = s, true
		}
	}
	return maxTS, found
}

// isDecorative reports whether a line carries no actual lyric text: blank, a
// run of music-note glyphs (including the "♪ Instrumental ♪" marker form), or
// an [key:value] metadata tag.
func isDecorative(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	if isNoteOnly(t) {
		return true
	}
	return isTagLine(t)
}

// noteGlyphs are the decorative music-note marks providers park on a section
// or instrumental line. Covering only ♪/♫ leaves the rest escaping the trim,
// which falsely demotes a perfectly-synced lyric -- the exact regression this
// package exists to prevent -- so the set spans the Miscellaneous Symbols notes
// and the two emoji forms.
var noteGlyphs = map[rune]bool{
	'♩':          true, // ♩ quarter note
	'♪':          true, // ♪ eighth note
	'♫':          true, // ♫ beamed eighth notes
	'♬':          true, // ♬ beamed sixteenth notes
	'\U0001F3B5': true, // 🎵 musical note
	'\U0001F3B6': true, // 🎶 multiple musical notes
}

// zeroWidth reports whether r is an invisible formatting mark that
// strings.TrimSpace does not consider a space (zero-width space, BOM). A line
// of nothing but these carries no lyric text.
func zeroWidth(r rune) bool {
	return r == '\u200b' || r == '\ufeff'
}

// isNoteOnly reports whether t consists solely of music-note glyphs, spaces,
// and the word "Instrumental" -- i.e. a decorative section marker rather than
// sung words.
func isNoteOnly(t string) bool {
	stripped := strings.TrimSpace(strings.Map(func(r rune) rune {
		if noteGlyphs[r] || zeroWidth(r) {
			return -1
		}
		return r
	}, t))
	if stripped == "" {
		return true
	}
	// The writer's marker form carries a word between the glyphs; treat only
	// that exact decorative word as non-lyric, so a sung line that happens to
	// begin with a glyph is still text-bearing.
	return strings.EqualFold(stripped, "instrumental")
}

// tagKeys is the closed set of LRC metadata keys treated as non-lyric. It is an
// ALLOWLIST rather than the shape test used by lrcnormalize.parseTag /
// lyrics.parseTagLine: those run over a raw LRC header block, where every
// [key:value] line really is a tag, while this predicate runs over arbitrary
// provider-supplied lyric text. Accepting any [key:value] there also swallows
// section headers -- a trailing one past the audio end would then be trimmed and
// a genuine overrun silently under-reported.
var tagKeys = map[string]bool{
	// Standard LRC header tags.
	"ar": true, "ti": true, "al": true, "au": true, "by": true,
	"length": true, "offset": true, "re": true, "ve": true, "tool": true,
	// canticle's own provenance tags (internal/lyrics writer.go / parser.go).
	// Keep in sync with what the writer emits: omitting one means a
	// canticle-written .lrc read back through a parse lane treats its own
	// header as lyric text, inflating the max and demoting its own words.
	"id": true, "isrc": true, "mbid": true, "source": true, "dv": true,
	"fetched": true,
}

// isTagLine reports whether t is a full-line [key:value] LRC metadata tag with a
// recognized key. A timestamp-shaped line ([01:23.45]...) can never match: its
// key is numeric and no numeric key is in the allowlist.
func isTagLine(t string) bool {
	if !strings.HasPrefix(t, "[") || !strings.HasSuffix(t, "]") {
		return false
	}
	inner := t[1 : len(t)-1]
	idx := strings.IndexByte(inner, ':')
	if idx <= 0 {
		return false
	}
	return tagKeys[strings.ToLower(strings.TrimSpace(inner[:idx]))]
}

// seconds converts a cue timestamp to seconds, preferring Total and falling
// back to the components when it is zero. Providers differ in which they
// populate: the musixmatch lane unmarshals Total straight from the payload,
// while a hand-built or LRC-parsed Synced may carry only components.
func seconds(t models.Time) float64 {
	if t.Total != 0 {
		return t.Total
	}
	return float64(t.Minutes*60+t.Seconds) + float64(t.Hundredths)/100.0
}
