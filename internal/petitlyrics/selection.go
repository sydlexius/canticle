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

// selectCandidate picks the best song from an API response.
//
// Precedence, strongest signal first:
//
//  1. ISRC exact match, when BOTH sides carry one. This is the same tier-1
//     identifier realign's resolver trusts. The provider's ISRC is sparse (of
//     two probe tracks, one carried a valid ISRC and one an empty field), so
//     this decides a minority of lookups and must degrade silently rather than
//     rejecting candidates that simply lack the field.
//  2. Duration agreement, using the shared 5-second bucket. Given ISRC
//     sparsity this is the workhorse signal, not a fallback.
//  3. Title and album textual similarity.
//
// Deliberately NOT used: availableLyricsType. It is not a capability set --
// both probe tracks reported the same value regardless of the tier requested --
// so tier preference is derived from the decoded payload instead.
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
	if track.TrackLength > 0 && s.DurationMS > 0 {
		want := normalize.DurationBucket(track.TrackLength)
		got := normalize.DurationBucket(s.DurationMS / 1000)
		switch diff := want - got; diff {
		case 0:
			score += 10
		case 1, -1:
			// One bucket apart is 5s, within normal tagging drift.
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
