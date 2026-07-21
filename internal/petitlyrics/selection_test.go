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
			// DurationBucket floors to 5s, so 210s and 214s share bucket 42 and
			// score identically. 216s is bucket 43 -- genuinely one bucket away.
			name:           "exact duration beats adjacent-bucket duration",
			stronger:       apiSong{DurationMS: 210000},
			weaker:         apiSong{DurationMS: 216000},
			wantStrongerHi: true,
		},
		{
			name:           "adjacent-bucket duration still scores above nothing",
			stronger:       apiSong{DurationMS: 216000},
			weaker:         apiSong{},
			wantStrongerHi: true,
		},
		{
			// Same-bucket durations are deliberately indistinguishable: the bucket
			// IS the tolerance, so a 4s difference must not break a tie.
			name:           "same-bucket durations tie",
			stronger:       apiSong{DurationMS: 210000},
			weaker:         apiSong{DurationMS: 214000},
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
