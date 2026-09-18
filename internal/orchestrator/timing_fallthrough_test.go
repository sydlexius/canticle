package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/innertube"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
)

// The query track's TrackLength is the AUDIO FILE's duration (the worker
// settles it from the file's own tags before dispatch), and it is the same
// value the writer's accept-time guard (#439) judges against. These fixtures
// are measured against 100s of audio.
const fallthroughAudioSeconds = 100

func timedLine(sec int, text string) models.Lines {
	return models.Lines{Text: text, Time: models.Time{Total: float64(sec), Minutes: sec / 60, Seconds: sec % 60}}
}

// categoricalSong is timed to a much longer recording: its last text cue sits
// at 4x the audio duration, which the guard quarantines (writes nothing).
func categoricalSong(body string) models.Song {
	return models.Song{Subtitles: models.Synced{Lines: []models.Lines{timedLine(10, body), timedLine(400, body)}}}
}

// overrunSong overruns the audio by more than the tolerance but stays under
// the categorical ratio: the right words, the wrong timing (demoted to .txt).
func overrunSong(body string) models.Song {
	return models.Song{Subtitles: models.Synced{Lines: []models.Lines{timedLine(10, body), timedLine(120, body)}}}
}

// goodSyncedSong fits the audio.
func goodSyncedSong(body string) models.Song {
	return models.Song{Subtitles: models.Synced{Lines: []models.Lines{timedLine(10, body), timedLine(90, body)}}}
}

func fallthroughTrack() models.Track {
	return models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title", TrackLength: fallthroughAudioSeconds}
}

func firstLine(s models.Song) string {
	if len(s.Subtitles.Lines) == 0 {
		return ""
	}
	return s.Subtitles.Lines[0].Text
}

func assertAttempts(t *testing.T, got []models.LaneAttempt, want map[string]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("lane attempts = %+v; want %d entries %v", got, len(want), want)
	}
	for _, a := range got {
		hit, ok := want[a.Lane]
		if !ok {
			t.Fatalf("unexpected lane attempt %+v; want %v", a, want)
		}
		if a.Hit != hit {
			t.Fatalf("lane %s hit = %v; want %v (attempts %+v)", a.Lane, a.Hit, hit, got)
		}
	}
}

// TestOrderedCategoricalFallsThroughToNextLane is the #950 regression: a
// result the timing guard would refuse is "this lane had nothing usable", so
// the next lane is consulted and its correctly timed lyric wins.
func TestOrderedCategoricalFallsThroughToNextLane(t *testing.T) {
	p1 := &stubProvider{name: "innertube", song: categoricalSong("wrong recording")}
	p2 := &stubProvider{name: "musixmatch", song: goodSyncedSong("right recording")}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if p2.calls != 1 {
		t.Fatalf("second lane calls = %d; want 1 (a categorical first result must not stop dispatch)", p2.calls)
	}
	if firstLine(song) != "right recording" || song.WinningLane != "musixmatch" {
		t.Fatalf("got %q from lane %q; want the second lane's correctly timed lyric", firstLine(song), song.WinningLane)
	}
	assertAttempts(t, song.LaneAttempts, map[string]bool{"innertube": false, "musixmatch": true})
}

// TestOrderedCategoricalExhaustedReturnsIt: with no lane holding anything
// better, the refused result is still returned (nil error) so the worker
// settles the row as it does today, with its timing verdict recorded.
func TestOrderedCategoricalExhaustedReturnsIt(t *testing.T) {
	p1 := &stubProvider{name: "innertube", song: categoricalSong("wrong recording")}
	p2 := &stubProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if p2.calls != 1 {
		t.Fatalf("second lane calls = %d; want 1", p2.calls)
	}
	if firstLine(song) != "wrong recording" || song.WinningLane != "innertube" {
		t.Fatalf("got %q from lane %q; want the categorical result as the last resort", firstLine(song), song.WinningLane)
	}
}

// TestOrderedCategoricalLosesToAnyLaterResult: a refused result writes
// nothing, so even a later provider instrumental marker outranks it.
func TestOrderedCategoricalLosesToAnyLaterResult(t *testing.T) {
	p1 := &stubProvider{name: "innertube", song: categoricalSong("wrong recording")}
	p2 := &stubProvider{name: "musixmatch", song: models.Song{Track: models.Track{Instrumental: 1}}}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "musixmatch" || song.Track.Instrumental != 1 {
		t.Fatalf("winner = %q (instrumental=%d); want the later instrumental over a write-nothing categorical", song.WinningLane, song.Track.Instrumental)
	}
}

// TestOrderedOverrunPrefersLaterGoodSynced: a MisSynced result would only
// land as .txt, so a later lane's correctly timed synced lyric is sought first.
func TestOrderedOverrunPrefersLaterGoodSynced(t *testing.T) {
	p1 := &stubProvider{name: "innertube", song: overrunSong("demotable")}
	p2 := &stubProvider{name: "musixmatch", song: goodSyncedSong("well timed")}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if firstLine(song) != "well timed" || song.WinningLane != "musixmatch" {
		t.Fatalf("got %q from %q; want the later well-timed synced lyric", firstLine(song), song.WinningLane)
	}
}

// TestOrderedOverrunIsTheFallback: when nothing better arrives, the demotable
// result is kept (the writer lands its words as .txt, exactly as before).
func TestOrderedOverrunIsTheFallback(t *testing.T) {
	p1 := &stubProvider{name: "innertube", song: overrunSong("demotable")}
	p2 := &stubProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if firstLine(song) != "demotable" || song.WinningLane != "innertube" {
		t.Fatalf("got %q from %q; want the MisSynced fallback", firstLine(song), song.WinningLane)
	}
	assertAttempts(t, song.LaneAttempts, map[string]bool{"innertube": true, "musixmatch": false})
}

// TestOrderedUnknownDurationFailsOpen: with no duration the guard cannot judge
// and promotes; the orchestrator must agree and stop at the first lane.
func TestOrderedUnknownDurationFailsOpen(t *testing.T) {
	p1 := &stubProvider{name: "innertube", song: categoricalSong("unjudgeable")}
	p2 := &stubProvider{name: "musixmatch", song: goodSyncedSong("never asked")}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))

	track := fallthroughTrack()
	track.TrackLength = 0
	song, err := o.FindLyrics(context.Background(), track, "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if p2.calls != 0 || song.WinningLane != "innertube" {
		t.Fatalf("second lane calls = %d, winner %q; want the first lane to win on unknown duration", p2.calls, song.WinningLane)
	}
}

// TestParallelCategoricalDoesNotBeatGoodSynced: a fast categorical synced
// result must not commit immediately and cancel a slower correctly timed one.
func TestParallelCategoricalDoesNotBeatGoodSynced(t *testing.T) {
	fast := &delayProvider{name: "innertube", song: categoricalSong("wrong recording")}
	slow := &delayProvider{name: "musixmatch", song: goodSyncedSong("right recording"), delay: 30 * time.Millisecond}
	o, _ := New(ModeParallel, delayLane(fast), delayLane(slow))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if firstLine(song) != "right recording" || song.WinningLane != "musixmatch" {
		t.Fatalf("got %q from %q; want the slower correctly timed lyric", firstLine(song), song.WinningLane)
	}
	assertAttempts(t, song.LaneAttempts, map[string]bool{"innertube": false, "musixmatch": true})
}

// TestOrderedOverrunBeatsGuardRejected is round-1 finding 1: a result the
// script guard rejects and a MisSynced one both land (at most) as .txt, but
// only the MisSynced one will actually be written -- the worker settles a
// guard-rejected result with nothing on disk. The suitable result must win
// regardless of which lane answered first.
func TestOrderedOverrunBeatsGuardRejected(t *testing.T) {
	guard := scriptOnlyGuard{accept: func(s models.Song) bool { return s.Lyrics.LyricsBody != "foreign" }}
	p1 := &stubProvider{name: "innertube", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "foreign"}}}
	p2 := &stubProvider{name: "musixmatch", song: overrunSong("right words")}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))
	o.SetGuard(guard)

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "musixmatch" || firstLine(song) != "right words" {
		t.Fatalf("winner %q (body %q, first line %q); want the MisSynced lyric over the guard-rejected one",
			song.WinningLane, song.Lyrics.LyricsBody, firstLine(song))
	}
}

// TestOrderedDetectorDoesNotOverrideOverrun is round-1 finding 2: a held
// MisSynced lyric is real words, so a later instrumental verdict must not
// replace it, and the detector (a YAMNet + ffmpeg run) is not invoked at all.
func TestOrderedDetectorDoesNotOverrideOverrun(t *testing.T) {
	p := &stubProvider{name: "innertube", song: overrunSong("real words")}
	d := &stubDetector{res: detector.Result{Instrumental: true, Confidence: 0.9, VocalConfidence: 0.01, SpeechConfidence: 0.02, Version: "1.5.0"}}
	o, _ := New(ModeOrdered, laneFor(p), NewDetectorLane(d, circuit.New(time.Minute, time.Hour), nil))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "/library/track.flac")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "innertube" || song.Track.Instrumental == 1 || firstLine(song) != "real words" {
		t.Fatalf("winner %q (instrumental=%d, first line %q); want the held MisSynced words", song.WinningLane, song.Track.Instrumental, firstLine(song))
	}
	if d.calls != 0 {
		t.Fatalf("detector calls = %d; want 0 (a held lyric makes the instrumental verdict moot)", d.calls)
	}
	assertAttempts(t, song.LaneAttempts, map[string]bool{"innertube": true})
}

// TestOrderedDetectorStillRunsAfterCategorical: a categorical result writes
// nothing, so it is not "a lyric held" and the detector keeps its turn.
func TestOrderedDetectorStillRunsAfterCategorical(t *testing.T) {
	p := &stubProvider{name: "innertube", song: categoricalSong("wrong recording")}
	d := &stubDetector{res: detector.Result{Instrumental: true, Confidence: 0.9, VocalConfidence: 0.01, SpeechConfidence: 0.02, Version: "1.5.0"}}
	o, _ := New(ModeOrdered, laneFor(p), NewDetectorLane(d, circuit.New(time.Minute, time.Hour), nil))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "/library/track.flac")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if d.calls != 1 || song.WinningLane != detectorLaneName {
		t.Fatalf("detector calls = %d, winner %q; want the detector consulted and its verdict landed", d.calls, song.WinningLane)
	}
}

// TestCategoricalWithUntriedLaneWaits: when the only result would be refused
// and some lane did NOT answer (rate limited, unauthorized, breaker open, not
// ready), the dispatch returns ErrTimingRefusedUntried carrying the refused
// song -- never the untried lane's own class, whose worker arms idle the whole
// drain pass -- in both dispatch modes.
func TestCategoricalWithUntriedLaneWaits(t *testing.T) {
	for _, mode := range []string{ModeOrdered, ModeParallel} {
		for _, tc := range []struct {
			name string
			lane func() *Lane
		}{
			{"rate limited", func() *Lane { return laneFor(&stubProvider{name: "musixmatch", err: musixmatch.ErrRateLimited}) }},
			{"unauthorized", func() *Lane { return laneFor(&stubProvider{name: "musixmatch", err: musixmatch.ErrUnauthorized}) }},
			{"breaker open", func() *Lane {
				open := circuit.New(time.Minute, time.Hour)
				open.Trip()
				return NewProviderLane(&stubProvider{name: "musixmatch", song: goodSyncedSong("never asked")}, open)
			}},
			{"not ready", func() *Lane { return laneFor(&stubProvider{name: "musixmatch", err: ErrLaneNotReady}) }},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				p1 := &stubProvider{name: "innertube", song: categoricalSong("wrong recording")}
				o, _ := New(mode, laneFor(p1), tc.lane())

				song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
				if !errors.Is(err, ErrTimingRefusedUntried) || ClassifyOutcome(err) != OutcomeRefusedUntried {
					t.Fatalf("err = %v (class %d); want ErrTimingRefusedUntried", err, ClassifyOutcome(err))
				}
				if song.WinningLane != "innertube" || firstLine(song) != "wrong recording" {
					t.Fatalf("carried %q from %q; want the refused lyric for the bounded settle", firstLine(song), song.WinningLane)
				}
			})
		}
	}
}

// TestCategoricalWithAnsweringLanesSettles: a transport failure (including a
// request-shape 403 and a stale client version) and a benign miss are
// ANSWERS, not untried lanes, so the refused result settles with a nil error as
// it did before #950 -- waiting does not fix a refused request shape.
func TestCategoricalWithAnsweringLanesSettles(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transport", errors.New("connection refused")},
		{"forbidden", innertube.ErrForbidden},
		{"client version", innertube.ErrClientVersion},
		{"benign miss", musixmatch.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p1 := &stubProvider{name: "innertube", song: categoricalSong("wrong recording")}
			p2 := &stubProvider{name: "musixmatch", err: tc.err}
			o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))

			song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
			if err != nil || song.WinningLane != "innertube" || firstLine(song) != "wrong recording" {
				t.Fatalf("got (%q from %q, %v); want the refused lyric with a nil error", firstLine(song), song.WinningLane, err)
			}
		})
	}
}

// TestGuardRejectedWithUntriedLaneSettles: only a TIMING refusal waits. A
// retained result the script guard rejected, or a later non-refused result that
// displaced a refused one, settles even while a lane did not answer.
func TestGuardRejectedWithUntriedLaneSettles(t *testing.T) {
	t.Run("script guard", func(t *testing.T) {
		p1 := &stubProvider{name: "innertube", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "foreign"}}}
		p2 := &stubProvider{name: "musixmatch", err: musixmatch.ErrRateLimited}
		o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))
		o.SetGuard(scriptOnlyGuard{accept: func(models.Song) bool { return false }})

		song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
		if err != nil || song.WinningLane != "innertube" {
			t.Fatalf("got (%q, %v); want the guard-rejected result with a nil error", song.WinningLane, err)
		}
	})
	t.Run("refused displaced by instrumental", func(t *testing.T) {
		p1 := &stubProvider{name: "innertube", song: categoricalSong("wrong recording")}
		p2 := &stubProvider{name: "petitlyrics", song: models.Song{Track: models.Track{Instrumental: 1}}}
		p3 := &stubProvider{name: "musixmatch", err: musixmatch.ErrRateLimited}
		o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2), laneFor(p3))

		song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
		if err != nil || song.WinningLane != "petitlyrics" {
			t.Fatalf("got (%q, %v); want the instrumental with a nil error", song.WinningLane, err)
		}
	})
	t.Run("instrumental then refused", func(t *testing.T) {
		// The refused result arrives AFTER an instrumental and is not retained,
		// so the kept result is the instrumental and nothing waits.
		p1 := &stubProvider{name: "petitlyrics", song: models.Song{Track: models.Track{Instrumental: 1}}}
		p2 := &stubProvider{name: "innertube", song: categoricalSong("wrong recording")}
		p3 := &stubProvider{name: "musixmatch", err: musixmatch.ErrRateLimited}
		o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2), laneFor(p3))

		song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
		if err != nil || song.WinningLane != "petitlyrics" {
			t.Fatalf("got (%q, %v); want the instrumental with a nil error", song.WinningLane, err)
		}
	})
}

// TestCategoricalWithDetectorBreakerOpenSettles (#950 review I2): an
// instrumental-only lane is never an untried lane. With the detector's breaker
// open, a refused result settles at once in both modes -- including for an
// item with detection disabled (empty sourcePath), which could never run it.
func TestCategoricalWithDetectorBreakerOpenSettles(t *testing.T) {
	for _, mode := range []string{ModeOrdered, ModeParallel} {
		for _, sourcePath := range []string{"/lib/a.flac", ""} {
			t.Run(mode+"/path="+sourcePath, func(t *testing.T) {
				open := circuit.New(time.Minute, time.Hour)
				open.Trip()
				det := NewDetectorLane(&stubDetector{}, open, nil)
				p1 := &stubProvider{name: "innertube", song: categoricalSong("wrong recording")}
				p2 := &stubProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
				o, _ := New(mode, laneFor(p1), laneFor(p2), det)

				song, err := o.FindLyrics(context.Background(), fallthroughTrack(), sourcePath)
				if err != nil || song.WinningLane != "innertube" {
					t.Fatalf("got (%q, %v); want the refused lyric with a nil error (a detector is never untried)", song.WinningLane, err)
				}
			})
		}
	}
	// The reviewer's sequence: a live detector outage trips its breaker on row
	// A; row B then sees it open. Neither row may wait on it.
	t.Run("outage then open breaker", func(t *testing.T) {
		cb := circuit.New(time.Minute, time.Hour)
		det := NewDetectorLane(&stubDetector{err: errors.Join(ErrLaneOutage, errors.New("dial refused"))}, cb, nil)
		p1 := &stubProvider{name: "innertube", song: categoricalSong("wrong recording")}
		p2 := &stubProvider{name: "musixmatch", err: musixmatch.ErrNotFound}
		o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2), det)
		for _, row := range []string{"/lib/a.flac", "/lib/b.flac", ""} {
			if _, err := o.FindLyrics(context.Background(), fallthroughTrack(), row); err != nil {
				t.Fatalf("row %q: err = %v; want nil (a detector outage or open breaker never holds a refused result)", row, err)
			}
		}
	})
}

// TestOrderedFirstHeldKeepsPriority: two demotable results; the first held
// keeps it, so the higher-priority lane wins.
func TestOrderedFirstHeldKeepsPriority(t *testing.T) {
	p1 := &stubProvider{name: "innertube", song: overrunSong("first lane")}
	p2 := &stubProvider{name: "musixmatch", song: overrunSong("second lane")}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil || song.WinningLane != "innertube" || firstLine(song) != "first lane" {
		t.Fatalf("got %q from %q (%v); want the first held lyric", firstLine(song), song.WinningLane, err)
	}
}

// TestParallelOverrunDoesNotBeatGoodSynced: a fast MisSynced result lands only
// as .txt, so it must not commit and cancel a slower well-timed synced one.
func TestParallelOverrunDoesNotBeatGoodSynced(t *testing.T) {
	fast := &delayProvider{name: "innertube", song: overrunSong("overrun")}
	slow := &delayProvider{name: "musixmatch", song: goodSyncedSong("right recording"), delay: 30 * time.Millisecond}
	o, _ := New(ModeParallel, delayLane(fast), delayLane(slow))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil || song.WinningLane != "musixmatch" || firstLine(song) != "right recording" {
		t.Fatalf("got %q from %q (%v); want the slower well-timed synced lyric", firstLine(song), song.WinningLane, err)
	}
}

// TestOrderedOverrunKeepsPriorityOverLaterUnsynced is round-1 finding 4: a
// higher-priority MisSynced lyric and a lower-priority plain unsynced one both
// land as .txt. The earlier lane wins the tie, as it did before #950.
func TestOrderedOverrunKeepsPriorityOverLaterUnsynced(t *testing.T) {
	p1 := &stubProvider{name: "innertube", song: overrunSong("first lane words")}
	p2 := &stubProvider{name: "musixmatch", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "second lane words"}}}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "innertube" || firstLine(song) != "first lane words" {
		t.Fatalf("winner %q; want the higher-priority lane on an equal-quality .txt tie", song.WinningLane)
	}
	assertAttempts(t, song.LaneAttempts, map[string]bool{"innertube": true, "musixmatch": false})
}

// TestParallelOverrunBeatsGuardRejected mirrors finding 1 under parallel
// dispatch, where the guard-rejected result can also arrive first.
func TestParallelOverrunBeatsGuardRejected(t *testing.T) {
	fast := &delayProvider{name: "innertube", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "foreign"}}}
	slow := &delayProvider{name: "musixmatch", song: overrunSong("right words"), delay: 20 * time.Millisecond}
	o, _ := New(ModeParallel, delayLane(fast), delayLane(slow))
	o.SetGuard(scriptOnlyGuard{accept: func(s models.Song) bool { return s.Lyrics.LyricsBody != "foreign" }})

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil || song.WinningLane != "musixmatch" {
		t.Fatalf("got (%q, %v); want the MisSynced lyric", song.WinningLane, err)
	}
}

// TestRetainedGuardRejectedRanksByWhatLands kills round-1 mutant M3: among
// retained (non-suitable) results, a MisSynced one ranks by what the writer
// would land (.txt), not by the synced cues the provider sent. It therefore
// ties a plain unsynced result, and the earlier lane keeps the tie.
func TestRetainedGuardRejectedRanksByWhatLands(t *testing.T) {
	guard := scriptOnlyGuard{accept: func(models.Song) bool { return false }}
	p1 := &stubProvider{name: "innertube", song: models.Song{Lyrics: models.Lyrics{LyricsBody: "first lane"}}}
	p2 := &stubProvider{name: "musixmatch", song: overrunSong("second lane")}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))
	o.SetGuard(guard)

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "innertube" {
		t.Fatalf("winner %q; want the earlier lane (a MisSynced result lands as .txt, tying an unsynced one)", song.WinningLane)
	}
}

// instrumentalWithSubtitle is a PROVIDER instrumental (no DetectorVersion) that
// also carries a subtitle line, which Musixmatch does. The writer treats the
// flag as authoritative and lands only an instrumental marker, so for ranking
// it is an instrumental result, whatever cues came with it.
func instrumentalWithSubtitle() models.Song {
	return models.Song{
		Track:     models.Track{Instrumental: 1},
		Subtitles: models.Synced{Lines: []models.Lines{timedLine(10, "instrumental cue")}},
	}
}

// TestOrderedHeldOverrunBeatsProviderInstrumentalWithSubtitle: a held
// MisSynced lyric is real words; a later provider instrumental must not
// replace it just because it carries a subtitle line.
func TestOrderedHeldOverrunBeatsProviderInstrumentalWithSubtitle(t *testing.T) {
	p1 := &stubProvider{name: "innertube", song: overrunSong("held words")}
	p2 := &stubProvider{name: "musixmatch", song: instrumentalWithSubtitle()}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "innertube" || song.Track.Instrumental == 1 || firstLine(song) != "held words" {
		t.Fatalf("winner %q (instrumental=%d, first line %q); want the held MisSynced words", song.WinningLane, song.Track.Instrumental, firstLine(song))
	}
	assertAttempts(t, song.LaneAttempts, map[string]bool{"innertube": true, "musixmatch": false})
}

// TestParallelHeldOverrunBeatsProviderInstrumentalWithSubtitle mirrors the
// ordered case: the slower instrumental must not commit over the held lyric.
func TestParallelHeldOverrunBeatsProviderInstrumentalWithSubtitle(t *testing.T) {
	fast := &delayProvider{name: "innertube", song: overrunSong("held words")}
	slow := &delayProvider{name: "musixmatch", song: instrumentalWithSubtitle(), delay: 30 * time.Millisecond}
	o, _ := New(ModeParallel, delayLane(fast), delayLane(slow))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "innertube" || song.Track.Instrumental == 1 || firstLine(song) != "held words" {
		t.Fatalf("winner %q (instrumental=%d, first line %q); want the held MisSynced words", song.WinningLane, song.Track.Instrumental, firstLine(song))
	}
}

// TestOrderedProviderInstrumentalWithSubtitleNothingHeldCommits pins the
// pre-existing behavior the fix must not touch: with nothing held, the
// instrumental-with-subtitle result still ends the dispatch at its lane.
func TestOrderedProviderInstrumentalWithSubtitleNothingHeldCommits(t *testing.T) {
	p1 := &stubProvider{name: "musixmatch", song: instrumentalWithSubtitle()}
	p2 := &stubProvider{name: "innertube", song: goodSyncedSong("never asked")}
	o, _ := New(ModeOrdered, laneFor(p1), laneFor(p2))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if p2.calls != 0 || song.WinningLane != "musixmatch" || song.Track.Instrumental != 1 {
		t.Fatalf("second lane calls = %d, winner %q (instrumental=%d); want the first lane's instrumental committed", p2.calls, song.WinningLane, song.Track.Instrumental)
	}
}

// TestParallelProviderInstrumentalWithSubtitleNothingHeldCommits: with nothing
// held, a fast instrumental-with-subtitle still commits immediately and cancels
// the slower lane, exactly as before.
func TestParallelProviderInstrumentalWithSubtitleNothingHeldCommits(t *testing.T) {
	fast := &delayProvider{name: "musixmatch", song: instrumentalWithSubtitle()}
	slow := &delayProvider{name: "innertube", song: goodSyncedSong("too late"), delay: time.Second}
	o, _ := New(ModeParallel, delayLane(fast), delayLane(slow))

	song, err := o.FindLyrics(context.Background(), fallthroughTrack(), "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.WinningLane != "musixmatch" || song.Track.Instrumental != 1 {
		t.Fatalf("winner %q (instrumental=%d); want the fast instrumental committed immediately", song.WinningLane, song.Track.Instrumental)
	}
	assertAttempts(t, song.LaneAttempts, map[string]bool{"musixmatch": true})
}
