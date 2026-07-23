package timing

import (
	"math"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// line builds a text-bearing cue at the given whole second.
func line(sec int, text string) models.Lines {
	return models.Lines{
		Text: text,
		Time: models.Time{Total: float64(sec), Minutes: sec / 60, Seconds: sec % 60},
	}
}

// componentLine builds a cue whose Total is deliberately left zero, exercising
// the Minutes/Seconds/Hundredths fallback.
func componentLine(m, s, h int, text string) models.Lines {
	return models.Lines{Text: text, Time: models.Time{Minutes: m, Seconds: s, Hundredths: h}}
}

func song(lines ...models.Lines) models.Song {
	return models.Song{Subtitles: models.Synced{Lines: lines}}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name     string
		song     models.Song
		duration int
		want     TimingOutcome
	}{
		{
			name:     "compliant lyric well inside the audio",
			song:     song(line(10, "a"), line(100, "b")),
			duration: 200,
			want:     Ok,
		},
		{
			name:     "long instrumental outro: far-negative overrun is fine",
			song:     song(line(10, "a"), line(60, "b")),
			duration: 400,
			want:     Ok,
		},
		{
			name:     "exactly at tolerance is Ok (boundary, inclusive)",
			song:     song(line(102, "a")),
			duration: 100,
			want:     Ok,
		},
		{
			name:     "just past tolerance is MisSynced",
			song:     song(line(103, "a")),
			duration: 100,
			want:     MisSynced,
		},
		{
			name:     "exactly at categorical ratio is Categorical (boundary, inclusive)",
			song:     song(line(150, "a")),
			duration: 100,
			want:     Categorical,
		},
		{
			name:     "just under categorical ratio stays MisSynced",
			song:     song(line(149, "a")),
			duration: 100,
			want:     MisSynced,
		},
		{
			name:     "far past the end is Categorical",
			song:     song(line(30, "a"), line(400, "b")),
			duration: 100,
			want:     Categorical,
		},
		{
			name:     "unknown duration fails open",
			song:     song(line(400, "a")),
			duration: 0,
			want:     UnknownDuration,
		},
		{
			name:     "negative duration fails open",
			song:     song(line(400, "a")),
			duration: -5,
			want:     UnknownDuration,
		},
		{
			name:     "empty subtitles are Ok",
			song:     song(),
			duration: 100,
			want:     Ok,
		},
		{
			name:     "all-decorative subtitles are Ok",
			song:     song(line(400, "♪"), line(500, "   ")),
			duration: 100,
			want:     Ok,
		},
		{
			name:     "component fallback when Total is zero",
			song:     song(componentLine(1, 43, 50, "a")),
			duration: 100,
			want:     MisSynced, // 103.5s vs 100s -> 3.5s overrun, ratio 1.035
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := Evaluate(tt.song, tt.duration)
			if got != tt.want {
				t.Errorf("Evaluate() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEvaluate_TrailingMarkerPastDurationIsOk is the Investigation-0 regression
// guard: ~31% of the raw flagged tail were perfectly-synced lyrics whose only
// past-duration timestamp was a trailing decorative marker. Keying on the raw
// last timestamp demotes them; the corrected max must not.
func TestEvaluate_TrailingMarkerPastDurationIsOk(t *testing.T) {
	trailing := []struct {
		name string
		text string
	}{
		{"bare music-note glyph", "♪"},
		{"repeated glyphs", "♪♪♪"},
		{"instrumental marker form", "♪ Instrumental ♪"},
		{"whitespace only", "   "},
		{"empty", ""},
		{"tag line", "[ar:Some Artist]"},
	}

	for _, tr := range trailing {
		t.Run(tr.name, func(t *testing.T) {
			// Last sung line at 90s sits comfortably inside a 100s track; the
			// decorative line at 160s would be a 60s overrun on the raw max.
			s := song(line(10, "a"), line(90, "b"), line(160, tr.text))
			got, mag := Evaluate(s, 100)
			if got != Ok {
				t.Errorf("Evaluate() = %q, want Ok (trailing %q must not count as overrun)", got, tr.name)
			}
			if mag.OverrunSeconds > 0 {
				t.Errorf("OverrunSeconds = %v, want <= 0 (max must be the last text-bearing line)", mag.OverrunSeconds)
			}
		})
	}
}

// TestEvaluate_MixedTrailingDecorativeIsTrimmedToLastText confirms trimming
// walks back over a run of mixed decorative kinds, not just one line.
func TestEvaluate_MixedTrailingDecorativeIsTrimmedToLastText(t *testing.T) {
	s := song(
		line(90, "last sung line"),
		line(150, "♪"),
		line(160, "  "),
		line(170, "[length:03:00]"),
		line(180, "♪ Instrumental ♪"),
	)
	got, mag := Evaluate(s, 100)
	if got != Ok {
		t.Errorf("Evaluate() = %q, want Ok", got)
	}
	if mag.OverrunSeconds != -10 {
		t.Errorf("OverrunSeconds = %v, want -10 (max should be the 90s line)", mag.OverrunSeconds)
	}
}

// TestEvaluate_DecorativeLineDoesNotSuppressARealOverrun guards the inverse of
// the regression above: trimming must not hide a genuinely MisSynced lyric
// whose last *text* line is itself past the end.
func TestEvaluate_DecorativeLineDoesNotSuppressARealOverrun(t *testing.T) {
	s := song(line(90, "a"), line(400, "real words past the end"), line(410, "♪"))
	got, mag := Evaluate(s, 100)
	if got != Categorical {
		t.Errorf("Evaluate() = %q, want Categorical", got)
	}
	if mag.Ratio != 4 {
		t.Errorf("Ratio = %v, want 4", mag.Ratio)
	}
}

func TestEvaluate_Magnitude(t *testing.T) {
	s := song(line(50, "a"), line(120, "b"))
	outcome, mag := Evaluate(s, 100)
	if outcome != MisSynced {
		t.Fatalf("outcome = %q, want MisSynced", outcome)
	}
	if mag.OverrunSeconds != 20 {
		t.Errorf("OverrunSeconds = %v, want 20", mag.OverrunSeconds)
	}
	if math.Abs(mag.Ratio-1.2) > 1e-9 {
		t.Errorf("Ratio = %v, want 1.2", mag.Ratio)
	}
}

func TestEvaluate_SentinelsCarryZeroMagnitude(t *testing.T) {
	for _, tc := range []struct {
		name     string
		song     models.Song
		duration int
	}{
		{"unknown duration", song(line(400, "a")), 0},
		{"no text-bearing line", song(line(400, "♪")), 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mag := Evaluate(tc.song, tc.duration)
			if mag != (Magnitude{}) {
				t.Errorf("Magnitude = %+v, want zero value", mag)
			}
		})
	}
}

func TestEvaluate_UnsortedCuesUseTheMaximum(t *testing.T) {
	// Cues arriving out of order must not let a late-but-early-listed line
	// escape the check.
	s := song(line(400, "late"), line(10, "early"))
	if got, _ := Evaluate(s, 100); got != Categorical {
		t.Errorf("Evaluate() = %q, want Categorical", got)
	}
}

// TestEvaluate_TotalTakesPrecedenceOverComponents pins the documented fallback
// order. Every other test builds cues whose Total and components agree, so
// without this the precedence is unverified: deleting the Total branch entirely
// left the suite green.
func TestEvaluate_TotalTakesPrecedenceOverComponents(t *testing.T) {
	disagreeing := models.Lines{
		Text: "a",
		// Total says 200s; the components say 60s. Total must win.
		Time: models.Time{Total: 200, Minutes: 1, Seconds: 0},
	}
	got, mag := Evaluate(models.Song{Subtitles: models.Synced{Lines: []models.Lines{disagreeing}}}, 100)
	if got != Categorical {
		t.Errorf("Evaluate() = %q, want Categorical (Total=200 vs duration=100)", got)
	}
	if mag.OverrunSeconds != 100 {
		t.Errorf("OverrunSeconds = %v, want 100 (Total must be preferred over components)", mag.OverrunSeconds)
	}
}

// TestEvaluate_HundredthsAreHonored guards the sub-second half of the component
// fallback, which the whole-second helpers never exercise.
func TestEvaluate_HundredthsAreHonored(t *testing.T) {
	_, mag := Evaluate(song(componentLine(1, 40, 75, "a")), 100)
	if mag.OverrunSeconds != 0.75 {
		t.Errorf("OverrunSeconds = %v, want 0.75 (100.75s - 100s)", mag.OverrunSeconds)
	}
}

// TestIsTagLine covers the tag-vs-lyric boundary directly. The allowlist exists
// because a bare [key:value] shape also matches section headers, and trimming
// one of those would under-report a real overrun.
func TestIsTagLine(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"[ar:Some Artist]", true},
		{"[ti:Some Title]", true},
		{"[length:03:00]", true},
		{"[source:canticle-detector]", true},
		{"[fetched:2026-07-23T00:00:00Z]", true},
		{"[AR:upper case key]", true},
		// Timestamp-shaped lines must never read as tags: a numeric key is not
		// in the allowlist.
		{"[01:23.45]", false},
		{"[123:45]", false},
		// Section headers are lyric-adjacent text, not metadata.
		{"[Chorus: 2x]", false},
		{"[Spoken: some words]", false},
		// Malformed or non-tag shapes.
		{"[]", false},
		{"[:]", false},
		{"[novalue]", false},
		{"not bracketed", false},
		{"[unclosed:", false},
	}
	for _, tt := range tests {
		if got := isTagLine(tt.text); got != tt.want {
			t.Errorf("isTagLine(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

// TestTagKeysCoverEveryTagCanticleWrites pins the allowlist against the writer.
// A key canticle emits but timing does not recognize makes a canticle-written
// .lrc, read back through a parse lane, treat its own header as lyric text --
// inflating the max and demoting its own words. That is how [fetched:] was
// missed, so the list is asserted rather than eyeballed.
func TestTagKeysCoverEveryTagCanticleWrites(t *testing.T) {
	// Keys emitted by internal/lyrics (writer.go tag block, parser.go
	// provenance round-trip). Extend this when the writer gains a tag.
	written := []string{
		"by", "ar", "ti", "al", "length", "ve",
		"source", "dv", "isrc", "mbid", "fetched",
	}
	for _, k := range written {
		if !tagKeys[k] {
			t.Errorf("tagKeys is missing %q, a tag canticle writes", k)
		}
	}
}

// TestEvaluate_SectionHeaderPastEndStillCountsAsOverrun is the inverse guard for
// the allowlist: a trailing section header is real text, so it must NOT be
// filtered away into a falsely-clean result.
func TestEvaluate_SectionHeaderPastEndStillCountsAsOverrun(t *testing.T) {
	s := song(line(90, "a"), line(400, "[Chorus: 2x]"))
	if got, _ := Evaluate(s, 100); got != Categorical {
		t.Errorf("Evaluate() = %q, want Categorical (a section header is not a metadata tag)", got)
	}
}

// TestEvaluate_InteriorDecorativeIsFiltered guards the filter-vs-trim
// distinction. Cue order is not guaranteed, so a decorative marker that is not
// in the trailing run must still be excluded from the max.
func TestEvaluate_InteriorDecorativeIsFiltered(t *testing.T) {
	// Only text-bearing lines are at 10s and 95s, both inside a 100s track.
	// The 400s glyph sits mid-slice, so a positional trim would miss it.
	s := song(line(10, "a"), line(400, "♪"), line(95, "b"))
	got, mag := Evaluate(s, 100)
	if got != Ok {
		t.Errorf("Evaluate() = %q, want Ok (interior decorative must not set the max)", got)
	}
	if mag.OverrunSeconds != -5 {
		t.Errorf("OverrunSeconds = %v, want -5 (max should be the 95s line)", mag.OverrunSeconds)
	}
}

// TestEvaluate_AllNoteGlyphVariants pins the glyph set. Each variant escaping
// the filter would falsely demote a perfectly-synced lyric.
func TestEvaluate_AllNoteGlyphVariants(t *testing.T) {
	for _, glyph := range []string{"♩", "♪", "♫", "♬", "\U0001F3B5", "\U0001F3B6", "\u200b", "\ufeff"} {
		t.Run(glyph, func(t *testing.T) {
			s := song(line(90, "a"), line(400, glyph))
			if got, _ := Evaluate(s, 100); got != Ok {
				t.Errorf("Evaluate() with trailing %q = %q, want Ok", glyph, got)
			}
		})
	}
}

// TestEvaluate_NonFiniteTimestampFailsOpen guards against a broken or hostile
// provider payload. Total is unmarshalled straight from JSON, and a NaN
// compares false against every threshold, so it would otherwise fall through to
// MisSynced and carry a NaN into downstream metrics.
func TestEvaluate_NonFiniteTimestampFailsOpen(t *testing.T) {
	for name, v := range map[string]float64{
		"NaN":  math.NaN(),
		"+Inf": math.Inf(1),
		"-Inf": math.Inf(-1),
	} {
		t.Run(name, func(t *testing.T) {
			s := song(models.Lines{Text: "a", Time: models.Time{Total: v}})
			got, mag := Evaluate(s, 100)
			// No cue carries a usable timestamp, so there is no timing evidence
			// to judge -- the same case as an all-decorative song.
			if got != Ok {
				t.Errorf("Evaluate() = %q, want Ok (fail open on non-finite)", got)
			}
			if mag != (Magnitude{}) {
				t.Errorf("Magnitude = %+v, want zero value", mag)
			}
		})
	}
}

// TestEvaluate_NonFiniteDoesNotMaskARealOverrun guards the sticky-NaN trap: NaN
// is never greater than anything, so a running maximum silently absorbs it and
// every later cue loses the comparison. Checking only the aggregate would let
// one bad cue suppress a genuine quarantine, and make the verdict depend on cue
// order -- which is not guaranteed.
func TestEvaluate_NonFiniteDoesNotMaskARealOverrun(t *testing.T) {
	real := models.Lines{Text: "real last line", Time: models.Time{Total: 400}}
	for name, v := range map[string]float64{
		"NaN":  math.NaN(),
		"+Inf": math.Inf(1),
		"-Inf": math.Inf(-1),
	} {
		bad := models.Lines{Text: "junk", Time: models.Time{Total: v}}
		for order, s := range map[string]models.Song{
			"bad first": song(bad, real),
			"bad last":  song(real, bad),
		} {
			t.Run(name+"/"+order, func(t *testing.T) {
				got, mag := Evaluate(s, 100)
				if got != Categorical {
					t.Errorf("Evaluate() = %q, want Categorical (the finite 400s cue must still win)", got)
				}
				if mag.OverrunSeconds != 300 {
					t.Errorf("OverrunSeconds = %v, want 300", mag.OverrunSeconds)
				}
			})
		}
	}
}

// TestEvaluate_ShortAudioToleranceFloor pins the deliberate precedence: the
// absolute tolerance is checked before the ratio, so a large ratio driven by a
// sub-tolerance overrun does not discard words.
func TestEvaluate_ShortAudioToleranceFloor(t *testing.T) {
	// 4s cue against a 3s track: ratio 1.33, but only a 1s overrun.
	if got, _ := Evaluate(song(line(4, "a")), 3); got != Ok {
		t.Errorf("Evaluate() = %q, want Ok (1s overrun is within Tolerance)", got)
	}

	// The discriminating case, and the doc comment's own worked example: a 5s
	// cue against a 3s track is ratio 1.67 -- at or beyond CategoricalRatio --
	// but only a 2s overrun. Tolerance is checked FIRST, so this is Ok. Without
	// this case, swapping the two switch arms passes the whole suite.
	got, mag := Evaluate(song(line(5, "a")), 3)
	if got != Ok {
		t.Errorf("Evaluate() = %q, want Ok (ratio %.2f is past CategoricalRatio, but the overrun is within Tolerance)", got, mag.Ratio)
	}
	if mag.Ratio < CategoricalRatio {
		t.Fatalf("test is vacuous: ratio %v must be >= CategoricalRatio to discriminate the ordering", mag.Ratio)
	}
}

// TestEvaluate_MeasuredFlag pins the distinction that keeps a persisted 0 from
// being confused with "no comparison made". A real Ok is Measured; both
// no-evidence cases (unknown duration, all-decorative lyric) are not, even
// though all three can surface OverrunSeconds 0.
func TestEvaluate_MeasuredFlag(t *testing.T) {
	tests := []struct {
		name         string
		song         models.Song
		duration     int
		wantOutcome  TimingOutcome
		wantMeasured bool
	}{
		{"real ok", song(line(50, "a")), 100, Ok, true},
		{"real overrun", song(line(120, "a")), 100, MisSynced, true},
		{"unknown duration", song(line(50, "a")), 0, UnknownDuration, false},
		{"all-decorative synced lyric", song(line(400, "♪"), line(500, "[ar:x]")), 100, Ok, false},
		{"empty subtitles", song(), 100, Ok, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, mag := Evaluate(tt.song, tt.duration)
			if outcome != tt.wantOutcome {
				t.Errorf("outcome = %q, want %q", outcome, tt.wantOutcome)
			}
			if mag.Measured != tt.wantMeasured {
				t.Errorf("Measured = %v, want %v", mag.Measured, tt.wantMeasured)
			}
			// When unmeasured, the numeric fields must be the zero value: a
			// consumer that ignores Measured would otherwise persist them.
			if !mag.Measured && (mag.OverrunSeconds != 0 || mag.Ratio != 0) {
				t.Errorf("unmeasured magnitude carries data: %+v", mag)
			}
		})
	}
}

func TestThresholdsMatchCalibration(t *testing.T) {
	// Pinned by the 28.7k-corpus recalibration on issue #438. Changing either
	// value invalidates that calibration.
	if Tolerance != 2 {
		t.Errorf("Tolerance = %v, want 2", Tolerance)
	}
	if CategoricalRatio != 1.5 {
		t.Errorf("CategoricalRatio = %v, want 1.5", CategoricalRatio)
	}
}
