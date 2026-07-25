package petitlyrics

import (
	"errors"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

func TestSelectCandidate_EmptyIsNotFound(t *testing.T) {
	if _, err := selectCandidate(nil, models.Track{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty candidate list should be ErrNotFound, got %v", err)
	}
}

// TestSelectCandidate_ISRCWins pins the strongest signal: an exact ISRC match
// must beat a candidate that looks better on every text and duration signal.
func TestSelectCandidate_ISRCWins(t *testing.T) {
	songs := []apiSong{
		{LyricsID: "decoy", Title: "Lorem Ipsum", Album: "Amet Consectetur", DurationMS: 210000},
		{LyricsID: "want", Title: "Totally Different", Album: "Other", DurationMS: 1000, ISRC: "ZZZZZ0000001"},
	}
	got, err := selectCandidate(songs, models.Track{
		TrackName: "Lorem Ipsum", AlbumName: "Amet Consectetur",
		TrackLength: 210, ISRC: "ZZZZZ0000001",
	})
	if err != nil {
		t.Fatalf("selectCandidate: %v", err)
	}
	if got.LyricsID != "want" {
		t.Errorf("ISRC match should win, got %q", got.LyricsID)
	}
}

// TestSelectCandidate_ISRCSparsityDegrades is the case the live probe surfaced:
// the provider's ISRC field is frequently empty. A candidate missing it must
// still be selectable rather than being rejected outright.
func TestSelectCandidate_ISRCSparsityDegrades(t *testing.T) {
	songs := []apiSong{
		{LyricsID: "wrong-duration", Title: "Lorem Ipsum", DurationMS: 120000},
		{LyricsID: "right-duration", Title: "Lorem Ipsum", DurationMS: 210000},
	}
	got, err := selectCandidate(songs, models.Track{
		TrackName: "Lorem Ipsum", TrackLength: 210, ISRC: "ZZZZZ0000001",
	})
	if err != nil {
		t.Fatalf("selectCandidate: %v", err)
	}
	if got.LyricsID != "right-duration" {
		t.Errorf("with no provider ISRC, duration should decide; got %q", got.LyricsID)
	}
}

func TestSelectCandidate_DurationTolerance(t *testing.T) {
	songs := []apiSong{
		{LyricsID: "far", Title: "Lorem Ipsum", DurationMS: 200000},
		{LyricsID: "near", Title: "Lorem Ipsum", DurationMS: 212000},
	}
	got, err := selectCandidate(songs, models.Track{TrackName: "Lorem Ipsum", TrackLength: 210})
	if err != nil {
		t.Fatalf("selectCandidate: %v", err)
	}
	if got.LyricsID != "near" {
		t.Errorf("nearest duration should win, got %q", got.LyricsID)
	}
}

// TestScoreCandidate_TitleFloor pins the floor DIRECTLY, on the score.
//
// Testing it through selectCandidate does not work: with two candidates the
// better title wins on relative score whether or not the floor exists, so the
// floor never decides the outcome and deleting it leaves such a test passing.
// (Verified by mutation -- that is exactly how the earlier version of this test
// was vacuous.) Scoring a single candidate makes the floor the only thing that
// can zero the contribution.
//
// "Ipsum Lorem" scores ~0.52 against "Lorem Ipsum" (same words, wrong order) and
// "Lorpsum" ~0.85, so the two straddle the 0.80 floor.
func TestScoreCandidate_TitleFloor(t *testing.T) {
	track := models.Track{TrackName: "Lorem Ipsum"}

	if got := scoreCandidate(apiSong{Title: "Ipsum Lorem"}, track); got != 0 {
		t.Errorf("a sub-floor title must contribute nothing, got score %v", got)
	}
	if got := scoreCandidate(apiSong{Title: "Lorpsum"}, track); got <= 0 {
		t.Errorf("an above-floor title must contribute, got score %v", got)
	}
	// And the floor must not swallow an exact match.
	if got := scoreCandidate(apiSong{Title: "Lorem Ipsum"}, track); got <= 0 {
		t.Errorf("an exact title match must contribute, got score %v", got)
	}
}

// TestSelectCandidate_SubFloorTitleLosesToAlbum: with the floor in force, a
// candidate whose only signal is a sub-floor title scores zero, so a candidate
// matching on album alone wins.
func TestSelectCandidate_SubFloorTitleLosesToAlbum(t *testing.T) {
	songs := []apiSong{
		{LyricsID: "subfloor-title", Title: "Ipsum Lorem"},
		{LyricsID: "album-match", Title: "Zzzz Qqqq", Album: "Amet Consectetur"},
	}
	got, err := selectCandidate(songs, models.Track{
		TrackName: "Lorem Ipsum", AlbumName: "Amet Consectetur",
	})
	if err != nil {
		t.Fatalf("selectCandidate: %v", err)
	}
	if got.LyricsID != "album-match" {
		t.Errorf("a sub-floor title contributes nothing, so the album match should win; got %q", got.LyricsID)
	}
}

// TestScoreCandidate_WeightOrdering pins the invariant scoreCandidate's doc
// asserts: a stronger signal can never be outvoted by a weaker one. Each case
// pairs a candidate winning on one signal against a candidate winning on every
// weaker signal combined.
func TestScoreCandidate_WeightOrdering(t *testing.T) {
	track := models.Track{
		TrackName: "Lorem Ipsum", AlbumName: "Amet Consectetur",
		TrackLength: 210, ISRC: "ZZZZZ0000001",
	}
	// Everything a weaker-signal candidate can possibly accumulate.
	maxWeaker := apiSong{Title: "Lorem Ipsum", Album: "Amet Consectetur", DurationMS: 210000}

	tests := []struct {
		name           string
		stronger       apiSong
		weaker         apiSong
		wantStrongerHi bool
	}{
		{
			name:           "ISRC beats duration+title+album combined",
			stronger:       apiSong{ISRC: "ZZZZZ0000001"},
			weaker:         maxWeaker,
			wantStrongerHi: true,
		},
		{
			name:           "exact duration beats title+album combined",
			stronger:       apiSong{DurationMS: 210000},
			weaker:         apiSong{Title: "Lorem Ipsum", Album: "Amet Consectetur"},
			wantStrongerHi: true,
		},
		{
			// Post-#639: scored on raw delta, not a fixed bucket. 216s is 6s away
			// from the 210s track, within the durationFarTolerance window (8s),
			// so it scores the lower tier (+5), allowing the exact match (+10) to win.
			name:           "exact duration beats far-tolerance duration",
			stronger:       apiSong{DurationMS: 210000},
			weaker:         apiSong{DurationMS: 216000},
			wantStrongerHi: true,
		},
		{
			name:           "far-tolerance duration still scores above nothing",
			stronger:       apiSong{DurationMS: 216000},
			weaker:         apiSong{},
			wantStrongerHi: true,
		},
		{
			// Both within the close tolerance (<=3s), so both score the same +10
			// tier -- neither should beat the other.
			name:           "durations within close tolerance tie",
			stronger:       apiSong{DurationMS: 210000},
			weaker:         apiSong{DurationMS: 212000},
			wantStrongerHi: false,
		},
		{
			name:           "title outweighs album",
			stronger:       apiSong{Title: "Lorem Ipsum"},
			weaker:         apiSong{Album: "Amet Consectetur"},
			wantStrongerHi: true,
		},
		{
			name:           "album similarity contributes something",
			stronger:       apiSong{Album: "Amet Consectetur"},
			weaker:         apiSong{},
			wantStrongerHi: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hi := scoreCandidate(tc.stronger, track)
			lo := scoreCandidate(tc.weaker, track)
			if (hi > lo) != tc.wantStrongerHi {
				t.Errorf("stronger=%.3f weaker=%.3f -- expected stronger>weaker to be %v",
					hi, lo, tc.wantStrongerHi)
			}
		})
	}
}

// TestScoreCandidate_DurationMonotonic_IssueCase pins the exact repro from
// #639: against a 210s local track, a candidate 4s away (214s) must not
// outscore a candidate 1s away (209s). Under the old DurationBucket-based
// scoring, 209s floors to bucket 41 (one bucket apart, +5) while 214s floors
// to bucket 42 (same bucket as 210s, +10) -- so the farther candidate won.
func TestScoreCandidate_DurationMonotonic_IssueCase(t *testing.T) {
	track := models.Track{TrackLength: 210}

	near := scoreCandidate(apiSong{DurationMS: 209000}, track) // 1s away
	far := scoreCandidate(apiSong{DurationMS: 214000}, track)  // 4s away

	if near < far {
		t.Errorf("209s (1s away) scored %v, 214s (4s away) scored %v -- closer candidate must not score lower", near, far)
	}
}

// TestScoreCandidate_DurationMonotonicity is the general invariant #639
// violated: over a range of local durations and deltas, a strictly closer
// candidate must never score lower than a strictly farther one. This is a
// property test, not a single example -- the bug was a quantization artifact
// that only shows up systematically across many local/delta combinations.
func TestScoreCandidate_DurationMonotonicity(t *testing.T) {
	for local := 60; local <= 900; local += 15 {
		track := models.Track{TrackLength: local}
		for closerDelta := -8; closerDelta <= 8; closerDelta++ {
			for fartherDelta := -8; fartherDelta <= 8; fartherDelta++ {
				if abs(fartherDelta) <= abs(closerDelta) {
					continue // only check pairs where farther is strictly farther
				}
				closerSec := local + closerDelta
				fartherSec := local + fartherDelta
				if closerSec <= 0 || fartherSec <= 0 {
					continue
				}
				closerScore := scoreCandidate(apiSong{DurationMS: closerSec * 1000}, track)
				fartherScore := scoreCandidate(apiSong{DurationMS: fartherSec * 1000}, track)
				if closerScore < fartherScore {
					t.Fatalf("local=%ds: candidate at delta %d (score %v) scored below candidate at delta %d (score %v) -- not monotonic",
						local, closerDelta, closerScore, fartherDelta, fartherScore)
				}
			}
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func TestSelectCandidate_SingleCandidateAlwaysReturned(t *testing.T) {
	// Even with no usable signals, one candidate is still a result: the provider
	// matched on the query it was given.
	songs := []apiSong{{LyricsID: "only"}}
	got, err := selectCandidate(songs, models.Track{})
	if err != nil {
		t.Fatalf("selectCandidate: %v", err)
	}
	if got.LyricsID != "only" {
		t.Errorf("got %q", got.LyricsID)
	}
}

func TestScoreCandidate_ISRCCaseInsensitive(t *testing.T) {
	hi := scoreCandidate(apiSong{ISRC: "zzzzz0000001"}, models.Track{ISRC: "ZZZZZ0000001"})
	if hi < 100 {
		t.Errorf("ISRC comparison should be case-insensitive, score=%v", hi)
	}
}
