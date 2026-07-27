package lyrics

import (
	"strings"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/timing"
)

// PromotionDecision is what the accept-time timing guard (#439) decided to do
// with a synced result at the moment of promotion. It is the ACTION, kept
// separate from timing.TimingOutcome, which is the VERDICT: two verdicts (Ok and
// UnknownDuration) map to the same action, and a non-synced song has an action
// but no verdict at all.
type PromotionDecision int

const (
	// PromoteAsIs writes what the content-type gate chose, unchanged. Every
	// non-synced result takes this path (there is no line timing to judge), as
	// does a synced result the predicate called Ok or UnknownDuration.
	PromoteAsIs PromotionDecision = iota
	// DemoteToUnsynced means the lyric runs past the audio by more than
	// timing.Tolerance but stays under timing.CategoricalRatio. The words are
	// content-correct (Investigation-0 on #438: a flagged overrun is the right
	// song's words with the wrong timing), so the .lrc is refused and the words
	// are kept as .txt -- unless a .txt is already settled on disk, which is left
	// alone.
	DemoteToUnsynced
	// Quarantine means the lyric is almost certainly timed to a different,
	// longer recording. Nothing is written and nothing on disk is disturbed.
	Quarantine
)

// String renders the decision for logs.
func (d PromotionDecision) String() string {
	switch d {
	case PromoteAsIs:
		return "promote"
	case DemoteToUnsynced:
		return "demote_to_unsynced"
	case Quarantine:
		return "quarantine"
	default:
		return "unknown"
	}
}

// DecidePromotion runs the accept-time timing guard for song and returns the
// action to take, plus the timing verdict and its magnitude for the caller to
// record (#440's dedicated columns).
//
// It DELEGATES the predicate to internal/timing and owns no comparison logic of
// its own. That is load-bearing, not stylistic: ~33% of naively-flagged files
// are perfectly-synced lyrics whose only past-duration timestamp is a trailing
// decorative marker, and timing.Evaluate is the one implementation that applies
// the corrected max (text-bearing lines only). A second max computed here would
// falsely demote a third of the flagged corpus, and timing's Tolerance and
// CategoricalRatio are calibrated against the corrected max and are not valid
// against any other.
//
// A non-synced song (instrumental, or unsynced-only) returns PromoteAsIs with an
// empty verdict: there is no line timing to evaluate, which is a distinct fact
// from a verdict of "fine". Callers persisting the verdict must leave the column
// NULL on an empty outcome, per migration 034's NULL semantics.
//
// The instrumental check mirrors WriteLRC's content-type gate and must come
// first: Musixmatch delivers a synced subtitle line alongside the instrumental
// flag, so a subtitles-first test would run the guard on a song whose .txt
// marker carries no timing at all.
func DecidePromotion(song models.Song) (PromotionDecision, timing.TimingOutcome, timing.Magnitude) {
	if song.Track.Instrumental == 1 || len(song.Subtitles.Lines) == 0 {
		return PromoteAsIs, "", timing.Magnitude{}
	}
	outcome, mag := timing.Evaluate(song, guardDurationSeconds(song))
	switch outcome {
	case timing.MisSynced:
		return DemoteToUnsynced, outcome, mag
	case timing.Categorical:
		return Quarantine, outcome, mag
	case timing.Ok, timing.UnknownDuration:
		return PromoteAsIs, outcome, mag
	default:
		// An outcome this package does not recognize must never reject a write.
		// Fail open and keep the verdict for the record.
		return PromoteAsIs, outcome, mag
	}
}

// guardDurationSeconds picks the duration the verdict is measured against.
//
// Song.AudioDurationSeconds is the ground truth (the file's own tags) and is
// what every caller inside canticle stamps. Track.TrackLength is the fallback
// for a song assembled without it: that value is the PROVIDER's catalog length
// on the fetch path, so the comparison is near-circular and only gross
// mismatches survive it. Falling back is still strictly better than failing open
// unconditionally, and a song with neither yields 0, which timing.Evaluate reads
// as UnknownDuration and fails open on.
func guardDurationSeconds(song models.Song) int {
	if song.AudioDurationSeconds > 0 {
		return song.AudioDurationSeconds
	}
	return song.Track.TrackLength
}

// GuardDurationSeconds is the duration the promotion guard judged this song
// against, exported so the worker's post-write record is stamped from the SAME
// value WriteLRC enforced on.
//
// Re-deriving a verdict from "the same inputs" is only safe when both sides
// genuinely share those inputs. They did not: the fallback above is applied
// here and was not applied at the stamp site, so a song with an unknown file
// duration but a known catalog length could be demoted on the fallback while
// the row recorded unknown_duration -- the durable record contradicting the
// decision it was supposed to document.
func GuardDurationSeconds(song models.Song) int {
	return guardDurationSeconds(song)
}

// unsyncedFallbackBody returns the plain words to persist when a synced result
// is demoted. The provider's own unsynced body wins when present: it is the
// authoritative plain text, correctly punctuated and line-broken. Otherwise the
// cues are flattened to their text, which loses only the (rejected) timing.
//
// Decorative cues are dropped via timing.IsDecorative -- the SAME predicate the
// verdict used -- so a demoted .txt does not gain stray music-note lines the
// unsynced body would never have carried, and the two can never disagree about
// what counts as text. An empty result means there is nothing worth writing and
// the caller keeps whatever is already on disk.
func unsyncedFallbackBody(song models.Song) string {
	if body := song.Lyrics.LyricsBody; body != "" {
		return body
	}
	var b strings.Builder
	for _, l := range song.Subtitles.Lines {
		text := strings.TrimSpace(l.Text)
		if timing.IsDecorative(text) {
			continue
		}
		b.WriteString(text)
		b.WriteString("\n")
	}
	return b.String()
}
