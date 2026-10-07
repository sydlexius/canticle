package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
)

// bodyBlocker blocks results whose first cue is in blocked, and records the
// identity key each check was made under.
type bodyBlocker struct {
	mu      sync.Mutex
	blocked map[string]bool
	keys    []string
}

func (b *bodyBlocker) SongBlocked(_ context.Context, s models.Song) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.keys = append(b.keys, s.IdentityArtistKey+"|"+s.IdentityTitleKey)
	return b.blocked[blockedKey(s)]
}

// blockedKey is the first cue, or the lyric body for a song with no subtitles.
func blockedKey(s models.Song) string {
	if len(s.Subtitles.Lines) == 0 {
		return s.Lyrics.LyricsBody
	}
	return firstLine(s)
}

func blockedOrch(t *testing.T, mode string, blocked []string, lanes ...*Lane) (*Orchestrator, *bodyBlocker) {
	t.Helper()
	o, err := New(mode, lanes...)
	if err != nil {
		t.Fatal(err)
	}
	b := &bodyBlocker{blocked: map[string]bool{}}
	for _, s := range blocked {
		b.blocked[s] = true
	}
	o.SetBlockChecker(b)
	return o, b
}

func TestBlockedResultFallsThroughToNextLane(t *testing.T) {
	for _, mode := range []string{ModeOrdered, ModeParallel} {
		t.Run(mode, func(t *testing.T) {
			p1 := &stubProvider{name: "innertube", song: goodSyncedSong("wrong words")}
			p2 := &stubProvider{name: "musixmatch", song: goodSyncedSong("right words")}
			o, _ := blockedOrch(t, mode, []string{"wrong words"}, laneFor(p1), laneFor(p2))

			song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
			if err != nil {
				t.Fatalf("FindLyrics: %v", err)
			}
			if firstLine(song) != "right words" || song.WinningLane != "musixmatch" {
				t.Fatalf("got %q from %q; want the other lane's different body", firstLine(song), song.WinningLane)
			}
			// A blocked answer is not a provider miss (#1394): the blocked lane is
			// not attributed at all, so neither lane_attempts nor provider_outcomes
			// can count it.
			assertAttempts(t, song.LaneAttempts, map[string]bool{"musixmatch": true})
		})
	}
}

func TestAllBlockedReturnsErrAllResultsBlocked(t *testing.T) {
	for _, mode := range []string{ModeOrdered, ModeParallel} {
		t.Run(mode, func(t *testing.T) {
			p1 := &stubProvider{name: "innertube", song: goodSyncedSong("wrong words")}
			p2 := &stubProvider{name: "musixmatch", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "wrong words"}}}
			o, _ := blockedOrch(t, mode, []string{"wrong words"}, laneFor(p1), laneFor(p2))

			song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
			if !errors.Is(err, ErrAllResultsBlocked) {
				t.Fatalf("err = %v; want ErrAllResultsBlocked", err)
			}
			if ClassifyOutcome(err) != OutcomeBenignMiss {
				t.Fatalf("class = %v; want a benign miss (no breaker, no global backoff)", ClassifyOutcome(err))
			}
			if firstLine(song) != "" || song.Lyrics.LyricsBody != "" {
				t.Fatalf("a blocked result was kept as the fallback song: %+v", song)
			}
		})
	}
}

// A failed lane's error wins over a blocked answer, a clean miss lets it settle
// the dispatch, and a timing-refused result is still returned as without the block.
func TestBlockedWithOtherLaneOutcomes(t *testing.T) {
	blocked := goodSyncedSong("wrong words")
	cases := []struct {
		name    string
		other   *stubProvider
		wantErr func(error) bool
		wantRet string // first line of the returned song
	}{
		{"failed lane", &stubProvider{name: "musixmatch", err: errors.New("boom")},
			func(e error) bool { return e != nil && !errors.Is(e, ErrAllResultsBlocked) }, ""},
		{"clean miss", &stubProvider{name: "musixmatch", err: musixmatch.ErrNotFound},
			func(e error) bool { return errors.Is(e, ErrAllResultsBlocked) }, ""},
		{"timing-refused", &stubProvider{name: "musixmatch", song: categoricalSong("refused words")},
			func(e error) bool { return e == nil }, "refused words"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := blockedOrch(t, ModeOrdered, []string{"wrong words"}, laneFor(&stubProvider{name: "innertube", song: blocked}), laneFor(tc.other))
			song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
			if !tc.wantErr(err) {
				t.Fatalf("err = %v", err)
			}
			if firstLine(song) != tc.wantRet {
				t.Fatalf("returned %q; want %q", firstLine(song), tc.wantRet)
			}
		})
	}
}

// The block key is the work-queue ROW identity carried on ctx, never the
// resolved query track and never the provider's returned artist/title.
func TestBlockKeyIsTheRowIdentity(t *testing.T) {
	s := goodSyncedSong("wrong words")
	s.Track = models.Track{ArtistName: "Provider Spelling", TrackName: "Provider Title"}
	o, b := blockedOrch(t, ModeOrdered, nil, laneFor(&stubProvider{name: "innertube", song: s}))
	ctx := lyrics.WithBlockIdentity(context.Background(), "row artist key", "row title key")
	if _, err := o.FindLyrics(ctx, fallthroughTrack(), ""); err != nil {
		t.Fatal(err)
	}
	if want := "row artist key|row title key"; len(b.keys) != 1 || b.keys[0] != want {
		t.Fatalf("checked under %q; want the row identity %q", b.keys, want)
	}
}

// A blocked answer plus a lane that did NOT answer parks just this row through
// the bounded-wait class (never the untried lane's own class, whose worker arm
// idles the whole drain pass); a detector outage leaves the all-blocked verdict
// standing (#1394).
func TestBlockedWithUntriedOrOutageLane(t *testing.T) {
	for _, mode := range []string{ModeOrdered, ModeParallel} {
		for _, tc := range []struct {
			name      string
			lane      func() *Lane
			wantBlock bool // ErrAllResultsBlocked rather than the parked class
		}{
			{"breaker open", func() *Lane {
				open := circuit.New(time.Minute, time.Hour)
				open.Trip()
				return NewProviderLane(&stubProvider{name: "musixmatch", song: goodSyncedSong("never asked")}, open)
			}, false},
			{"throttled", func() *Lane { return laneFor(&stubProvider{name: "musixmatch", err: musixmatch.ErrRateLimited}) }, false},
			{"lane outage", func() *Lane { return laneFor(&stubProvider{name: "detector", err: ErrLaneOutage}) }, true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				o, _ := blockedOrch(t, mode, []string{"wrong words"}, laneFor(&stubProvider{name: "innertube", song: goodSyncedSong("wrong words")}), tc.lane())
				song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
				if tc.wantBlock {
					if !errors.Is(err, ErrAllResultsBlocked) {
						t.Fatalf("err = %v; want ErrAllResultsBlocked", err)
					}
					return
				}
				var ru *RefusedUntriedError
				if !errors.As(err, &ru) || !ru.Blocked || ClassifyOutcome(err) != OutcomeRefusedUntried {
					t.Fatalf("err = %v (class %d); want a Blocked RefusedUntriedError", err, ClassifyOutcome(err))
				}
				if song.Lyrics.LyricsBody != "" || len(song.Subtitles.Lines) != 0 {
					t.Fatalf("a blocked result was carried back: %+v", song)
				}
			})
		}
	}
}

// Blocked + a failed lane + an untried lane: the failed lane's error wins over
// the parked class, and a hollow response never becomes all-blocked (#1394).
func TestBlockedFailedAndHollowPrecedence(t *testing.T) {
	open := func() *Lane {
		cb := circuit.New(time.Minute, time.Hour)
		cb.Trip()
		return NewProviderLane(&stubProvider{name: "petitlyrics", song: goodSyncedSong("never asked")}, cb)
	}
	boom := errors.New("boom")
	hollow := fmt.Errorf("x: %w", musixmatch.ErrTruncatedResponse)
	blockedLane := func() *Lane {
		return laneFor(&stubProvider{name: "innertube", song: goodSyncedSong("wrong words")})
	}
	for _, mode := range []string{ModeOrdered, ModeParallel} {
		t.Run(mode+"/failed+untried", func(t *testing.T) {
			o, _ := blockedOrch(t, mode, []string{"wrong words"}, blockedLane(),
				laneFor(&stubProvider{name: "musixmatch", err: boom}), open())
			_, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
			var ru *RefusedUntriedError
			if !errors.Is(err, boom) || errors.As(err, &ru) {
				t.Fatalf("err = %v; want the failed lane's error, not a parked RefusedUntriedError", err)
			}
		})
		t.Run(mode+"/hollow", func(t *testing.T) {
			o, _ := blockedOrch(t, mode, []string{"wrong words"}, blockedLane(),
				laneFor(&stubProvider{name: "musixmatch", err: hollow}))
			_, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
			if !errors.Is(err, musixmatch.ErrTruncatedResponse) || errors.Is(err, ErrAllResultsBlocked) {
				t.Fatalf("err = %v; want the truncated-response error, not ErrAllResultsBlocked", err)
			}
		})
	}
}
