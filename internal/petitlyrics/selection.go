package petitlyrics

import (
	"strings"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
)

// titleMatchFloor is the minimum Jaro-Winkler confidence a candidate's title
// must reach before it can be chosen on text alone. Below this the candidate is
// treated as a different song rather than a fuzzy match.
const titleMatchFloor = 0.80

// durationCloseTolerance and durationFarTolerance are the two tiers of the
// duration-agreement term (see scoreCandidate), measured directly against the
// absolute per-second delta between the local track and a candidate rather
// than through normalize.DurationBucket (see #639: bucket equality is a fixed
// floor("seconds/5") grid, not a window centered on the local track, so
// comparing bucket numbers is not monotonic in the actual distance -- a
// candidate 4s away could out-score one 1s away).
//
// These are NOT copied from timing.Tolerance (2.0s): that constant answers a
// different question (how much a synced lyric may overrun the audio's actual
// end, calibrated against a 28.7k-track corpus) and #639 explicitly rules out
// assuming it transfers to provider-vs-file duration agreement.
//
// durationCloseTolerance=3s: the width a same-recording duration disagreement
// should realistically have from tagging/reporting drift alone -- taggers and
// providers round to the nearest second and some further round-trip through a
// 1-2s encoder/container estimate, so up to ~3s of drift is ordinary noise,
// not evidence of a different recording.
//
// durationFarTolerance=8s: the outer edge of "still plausibly the same
// recording, just less certain than a close match". This preserves the shape
// of the value it replaces -- the old two-tier code's second tier ("one
// bucket apart") could in the worst case span up to just under 10s (see
// issue #639) -- rounded down to 8s so there is a clear gap below the kind of
// gap a genuinely different edit (radio edit vs. album cut, live version)
// typically introduces, which tends to run well past 10s.
const (
	durationCloseTolerance = 3
	durationFarTolerance   = 8
)

// selectCandidate picks the best song from an API response.
//
// Precedence, strongest signal first:
//
//  1. ISRC exact match, when BOTH sides carry one. This is the same tier-1
//     identifier realign's resolver trusts. The provider's ISRC is sparse (of
//     two probe tracks, one carried a valid ISRC and one an empty field), so
//     this decides a minority of lookups and must degrade silently rather than
//     rejecting candidates that simply lack the field.
//  2. Duration agreement, measured on the raw per-second delta (see
//     durationCloseTolerance/durationFarTolerance below). Given ISRC
//     sparsity this is the workhorse signal, not a fallback.
//  3. Title and album textual similarity.
//
// Deliberately NOT used: availableLyricsType. Not because it is unreliable (an
// earlier comment said so from a two-track sample; that is RETRACTED, and over
// 107 hits it predicts the returned tier exactly), but because selection ranks
// CANDIDATES for identity match, and the tier a candidate offers is not evidence
// that it is the right recording.
//
// An empty candidate list returns ErrNotFound.
func selectCandidate(songs []apiSong, track models.Track) (apiSong, error) {
	if len(songs) == 0 {
		return apiSong{}, ErrNotFound
	}

	best := -1
	bestScore := -1.0
	for i, s := range songs {
		score := scoreCandidate(s, track)
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return songs[best], nil
}

// scoreCandidate ranks one candidate against the local track. Higher is better.
// The weights are ordered so that a signal can never be outvoted by a weaker
// one: an ISRC match dominates any combination of duration and text.
func scoreCandidate(s apiSong, track models.Track) float64 {
	var score float64

	// 1. ISRC: exact, case-insensitive, only when both sides have one.
	if track.ISRC != "" && s.ISRC != "" {
		if strings.EqualFold(strings.TrimSpace(track.ISRC), strings.TrimSpace(s.ISRC)) {
			score += 100
		}
	}

	// 2. Duration: the provider reports milliseconds, the local track seconds.
	// Scored on the raw absolute delta rather than normalize.DurationBucket --
	// see the durationCloseTolerance/durationFarTolerance doc comment for why.
	if track.TrackLength > 0 && s.DurationMS > 0 {
		delta := track.TrackLength - s.DurationMS/1000
		if delta < 0 {
			delta = -delta
		}
		switch {
		case delta <= durationCloseTolerance:
			score += 10
		case delta <= durationFarTolerance:
			score += 5
		}
	}

	// 3. Text similarity. Title carries more weight than album, and a title that
	// falls below the floor contributes nothing rather than a small positive.
	if track.TrackName != "" && s.Title != "" {
		if c := normalize.MatchConfidence(track.TrackName, s.Title); c >= titleMatchFloor {
			score += 3 * c
		}
	}
	if track.AlbumName != "" && s.Album != "" {
		score += normalize.MatchConfidence(track.AlbumName, s.Album)
	}

	return score
}
