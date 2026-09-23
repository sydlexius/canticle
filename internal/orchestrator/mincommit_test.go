package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
)

// lineSyncedAnswer fits the audio (so the timing guard promotes it as-is) and
// carries the given word answer; wordSyncedAnswer adds served word timings.
func lineSyncedAnswer(body string, a models.WordAnswer) models.Song {
	s := goodSyncedSong(body)
	s.WordAnswer = a
	return s
}

func wordSyncedAnswer(body string) models.Song {
	s := lineSyncedAnswer(body, models.WordAnswerServed)
	s.WordTimings = []models.WordTiming{{Line: 0, Text: body, StartMS: 10000, EndMS: 11000}}
	return s
}

func wordOrchestrator(t *testing.T, gate Quality, ps ...*stubProvider) *Orchestrator {
	t.Helper()
	lanes := make([]*Lane, len(ps))
	for i, p := range ps {
		lanes[i] = laneFor(p)
	}
	o, err := New(ModeOrdered, lanes...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := o.SetMinCommitQuality(gate); err != nil {
		t.Fatalf("SetMinCommitQuality: %v", err)
	}
	return o
}

var wordTrack = models.Track{TrackLength: fallthroughAudioSeconds}

// TestMinCommit_LineSyncedFirstLaneDoesNotEndGatedDispatch is the gate itself:
// ungated, the line-synced first lane wins and the second is never asked;
// gated at word-synced, the dispatch continues and the later word lane wins.
func TestMinCommit_LineSyncedFirstLaneDoesNotEndGatedDispatch(t *testing.T) {
	for _, tc := range []struct {
		gate      Quality
		wantLane  string
		wantCalls int
	}{
		{QualityNone, providers.Musixmatch, 0},
		{QualityWordSynced, providers.PetitLyrics, 1},
	} {
		mxm := &stubProvider{name: providers.Musixmatch, song: lineSyncedAnswer("line", models.WordAnswerAbsent)}
		pl := &stubProvider{name: providers.PetitLyrics, song: wordSyncedAnswer("word")}
		song, err := wordOrchestrator(t, tc.gate, mxm, pl).FindLyrics(context.Background(), wordTrack, "")
		if err != nil {
			t.Fatalf("gate %d: %v", tc.gate, err)
		}
		if song.WinningLane != tc.wantLane || pl.calls != tc.wantCalls {
			t.Errorf("gate %d: winner %q after %d second-lane calls, want %q after %d",
				tc.gate, song.WinningLane, pl.calls, tc.wantLane, tc.wantCalls)
		}
		if tc.gate == QualityWordSynced && (song.WordAnswer != models.WordAnswerServed || len(song.LaneAttempts) != 2) {
			t.Errorf("gated winner: answer %q, %d attempts; want served over 2", song.WordAnswer, len(song.LaneAttempts))
		}
	}
}

// TestMinCommit_GatedEarlyWinnerAggregatesWordAnswer: under a synced gate the
// first lane's synced result commits at once, before the second word lane is
// asked. Its own absent must not survive as terminal: it becomes the aggregate,
// unknown here because only one of two word lanes answered.
func TestMinCommit_GatedEarlyWinnerAggregatesWordAnswer(t *testing.T) {
	mxm := &stubProvider{name: providers.Musixmatch, song: lineSyncedAnswer("line", models.WordAnswerAbsent)}
	pl := &stubProvider{name: providers.PetitLyrics, song: wordSyncedAnswer("word")}
	song, err := wordOrchestrator(t, QualitySynced, mxm, pl).FindLyrics(context.Background(), wordTrack, "")
	if err != nil {
		t.Fatal(err)
	}
	if song.WinningLane != providers.Musixmatch || pl.calls != 0 {
		t.Fatalf("winner %q after %d second-lane calls; want musixmatch after 0", song.WinningLane, pl.calls)
	}
	if song.WordAnswer != models.WordAnswerUnknown {
		t.Errorf("early gated winner word answer = %q; want unknown (a word lane was never asked)", song.WordAnswer)
	}
}

// TestMinCommit_NoWordsFallsBackToFirstLineSynced: with no lane reaching the
// gate, the kept line-synced result is returned, the earlier lane on a tie.
// Its word answer is the aggregate: absent only when every word lane answered.
func TestMinCommit_NoWordsFallsBackToFirstLineSynced(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second *stubProvider
		want   models.WordAnswer
	}{
		{"both answered no words", &stubProvider{name: providers.PetitLyrics, song: lineSyncedAnswer("second", models.WordAnswerAbsent)}, models.WordAnswerAbsent},
		{"second could not say", &stubProvider{name: providers.PetitLyrics, song: lineSyncedAnswer("second", models.WordAnswerUnknown)}, models.WordAnswerUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := &stubProvider{name: providers.Musixmatch, song: lineSyncedAnswer("first", models.WordAnswerAbsent)}
			song, err := wordOrchestrator(t, QualityWordSynced, first, tc.second).FindLyrics(context.Background(), wordTrack, "")
			if err != nil {
				t.Fatalf("FindLyrics: %v", err)
			}
			if song.WinningLane != providers.Musixmatch || song.Subtitles.Lines[0].Text != "first" {
				t.Errorf("fallback = %q from %q, want the first lane's line-synced result", song.Subtitles.Lines[0].Text, song.WinningLane)
			}
			if song.WordAnswer != tc.want {
				t.Errorf("WordAnswer = %q, want %q", song.WordAnswer, tc.want)
			}
		})
	}
}

// TestMinCommit_UngatedLeavesWordAnswerAlone: without a gate the lane's own
// unknown (or served) passes through untouched; only an absent is aggregated
// (TestUngated_AbsentIsTheAggregate).
func TestMinCommit_UngatedLeavesWordAnswerAlone(t *testing.T) {
	mxm := &stubProvider{name: providers.Musixmatch, song: lineSyncedAnswer("line", models.WordAnswerUnknown)}
	pl := &stubProvider{name: providers.PetitLyrics, err: musixmatch.ErrNotFound}
	song, err := wordOrchestrator(t, QualityNone, mxm, pl).FindLyrics(context.Background(), wordTrack, "")
	if err != nil || song.WordAnswer != models.WordAnswerUnknown {
		t.Fatalf("got (%q, %v), want the lane's unknown answer untouched", song.WordAnswer, err)
	}
}

// TestMinCommit_OnlyAGenuineNoMatchAnswers: only a genuine no-match answers the
// word question; any other miss, a breaker skip or a transport failure is unknown.
func TestMinCommit_OnlyAGenuineNoMatchAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want models.WordAnswer
	}{
		{"musixmatch not found", musixmatch.ErrNotFound, models.WordAnswerAbsent},
		{"petitlyrics no songs", petitlyrics.ErrNoMatch, models.WordAnswerAbsent},
		{"truncated body", musixmatch.ErrTruncatedResponse, models.WordAnswerUnknown},
		{"unparsable subtitle body", musixmatch.ErrUnparsableSubtitleBody, models.WordAnswerUnknown},
		{"petitlyrics decode failure", fmt.Errorf("decode: %w", petitlyrics.ErrNotFound), models.WordAnswerUnknown},
		{"petitlyrics outage", petitlyrics.ErrProviderUnavailable, models.WordAnswerUnknown},
		{"breaker open", ErrLaneUnavailable, models.WordAnswerUnknown},
		{"transport 5xx", errors.New("musixmatch API error: status 503"), models.WordAnswerUnknown},
		{"throttled", musixmatch.ErrUnauthorized, models.WordAnswerUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := &stubProvider{name: providers.Musixmatch, song: lineSyncedAnswer("first", models.WordAnswerAbsent)}
			second := &stubProvider{name: providers.PetitLyrics, err: tc.err}
			song, err := wordOrchestrator(t, QualityWordSynced, first, second).FindLyrics(context.Background(), wordTrack, "")
			if err != nil || song.WinningLane != providers.Musixmatch || song.WordAnswer != tc.want {
				t.Errorf("got %q from %q (%v), want %q from the first lane", song.WordAnswer, song.WinningLane, err, tc.want)
			}
		})
	}
}

// TestMinCommit_GatedResultSkipsDetector: a kept below-gate result skips the detector.
func TestMinCommit_GatedResultSkipsDetector(t *testing.T) {
	d := &stubDetector{}
	o := wordOrchestrator(t, QualityWordSynced, &stubProvider{name: providers.Musixmatch, song: goodSyncedSong("line")})
	o.lanes = append(o.lanes, NewDetectorLane(d, circuit.New(time.Minute, time.Hour), nil))
	if _, err := o.FindLyrics(context.Background(), wordTrack, "/a.flac"); err != nil || d.calls != 0 {
		t.Fatalf("err %v, detector calls %d; want nil and 0", err, d.calls)
	}
}

// TestMinCommit_GatedResultBeatsLaterHeld: a later held lyric never displaces an
// earlier gated result, including on an unsynced tie.
func TestMinCommit_GatedResultBeatsLaterHeld(t *testing.T) {
	unsynced := models.Song{Lyrics: models.Lyrics{LyricsBody: "first"}}
	for name, first := range map[string]models.Song{"synced": goodSyncedSong("first"), "unsynced tie": unsynced} {
		mxm := &stubProvider{name: providers.Musixmatch, song: first}
		pl := &stubProvider{name: providers.PetitLyrics, song: overrunSong("held")}
		song, err := wordOrchestrator(t, QualityWordSynced, mxm, pl).FindLyrics(context.Background(), wordTrack, "")
		if err != nil || song.WinningLane != providers.Musixmatch || pl.calls != 1 {
			t.Errorf("%s: winner %q (err %v, held-lane calls %d), want the earlier gated lane", name, song.WinningLane, err, pl.calls)
		}
	}
}

func TestMinCommit_RejectsOutOfRangeGate(t *testing.T) {
	o := wordOrchestrator(t, QualityNone, &stubProvider{name: providers.Musixmatch})
	for _, q := range []Quality{QualityNone - 1, QualityWordSynced + 1} {
		if err := o.SetMinCommitQuality(q); err == nil || o.minCommit != QualityNone {
			t.Errorf("SetMinCommitQuality(%d) accepted (err %v, gate %d)", q, err, o.minCommit)
		}
	}
}

func TestMinCommit_ParallelRefusesGate(t *testing.T) {
	o, err := New(ModeParallel, laneFor(&stubProvider{name: providers.Musixmatch}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := o.SetMinCommitQuality(QualityWordSynced); err == nil {
		t.Error("parallel mode accepted a commit gate")
	}
	if err := o.SetMinCommitQuality(QualityNone); err != nil {
		t.Errorf("parallel mode refused the no-gate default: %v", err)
	}
}

func TestLaneWordCapable(t *testing.T) {
	cases := map[*Lane]bool{
		laneFor(&stubProvider{name: providers.Musixmatch}):  true,
		laneFor(&stubProvider{name: providers.PetitLyrics}): true,
		laneFor(&stubProvider{name: providers.InnerTube}):   false,
		NewDetectorLane(&stubDetector{}, nil, nil):          false,
	}
	for l, want := range cases {
		if got := l.WordCapable(); got != want {
			t.Errorf("lane %q WordCapable = %v, want %v", l.Name(), got, want)
		}
	}
}

// TestUngated_AbsentIsTheAggregate (#982 slice 4): an ORDINARY dispatch's
// absent is the aggregate in both modes, since the worker stamps it: a lane's
// own absent survives only when every word-capable lane answered.
func TestUngated_AbsentIsTheAggregate(t *testing.T) {
	absent := func() *stubProvider {
		return &stubProvider{name: providers.Musixmatch, song: lineSyncedAnswer("line", models.WordAnswerAbsent)}
	}
	for _, mode := range []string{ModeOrdered, ModeParallel} {
		for _, tc := range []struct {
			name   string
			others []*stubProvider
			want   models.WordAnswer
		}{
			{"sole word lane", nil, models.WordAnswerAbsent},
			{"non-word lane beside it", []*stubProvider{{name: providers.InnerTube, err: musixmatch.ErrNotFound}}, models.WordAnswerAbsent},
			{"word lane that did not answer", []*stubProvider{{name: providers.PetitLyrics, err: petitlyrics.ErrRateLimited}}, models.WordAnswerUnknown},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				lanes := []*Lane{laneFor(absent())}
				for _, p := range tc.others {
					lanes = append(lanes, laneFor(p))
				}
				o, err := New(mode, lanes...)
				if err != nil {
					t.Fatal(err)
				}
				song, err := o.FindLyrics(context.Background(), wordTrack, "")
				if err != nil || song.WinningLane != providers.Musixmatch || song.WordAnswer != tc.want {
					t.Fatalf("got %q from %q (%v); want %q", song.WordAnswer, song.WinningLane, err, tc.want)
				}
			})
		}
	}
}
