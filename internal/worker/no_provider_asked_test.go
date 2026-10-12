package worker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/queue"
)

// A row no provider was asked about is not a miss (#1502): with every provider
// breaker open, the detector's "not instrumental" is the only answer, and it
// says nothing about the catalog. Every artist, title and album is invented.

// noAskCell is one providers.mode / instrumental_detector.ordering pair.
type noAskCell struct{ mode, ordering string }

func (c noAskCell) String() string { return c.mode + "/" + c.ordering }

// staged reports the one cell where the detector cannot answer before a
// provider has: parallel with the detector last (#528).
func (c noAskCell) staged() bool {
	return c.mode == orchestrator.ModeParallel && c.ordering != "front"
}

var noAskCells = []noAskCell{
	{orchestrator.ModeOrdered, "demoted"},
	{orchestrator.ModeOrdered, "front"},
	{orchestrator.ModeParallel, "demoted"},
	{orchestrator.ModeParallel, "front"},
}

// newNoAskRig is the title rig with a detector answering verdict, in cell c.
// Both providers miss when asked.
func newNoAskRig(t *testing.T, c noAskCell, track models.Track, verdict bool) (*titleRig, *fakeDetector) {
	t.Helper()
	r := newTitleRig(t, track)
	det := &fakeDetector{result: detector.Result{Instrumental: verdict, Version: "9.9.9", Reusable: true}}
	r.w.EnableAudioDetector(det)
	r.w.SetInstrumentalDetectionDefault(true)
	r.w.SetProvidersMode(c.mode)
	r.w.SetDetectorOrdering(c.ordering)
	r.primary.err = musixmatch.ErrNoLyrics
	return r, det
}

// tripProviders opens the first n provider breakers (musixmatch, then
// petitlyrics). Called last: the mode and ordering setters rebuild the lanes.
func (r *titleRig) tripProviders(t *testing.T, n int) {
	t.Helper()
	if len(r.w.lanes) != 2 {
		t.Fatalf("provider lanes = %d; the rig needs 2", len(r.w.lanes))
	}
	for _, l := range r.w.lanes[:n] {
		l.Breaker().Trip()
	}
}

// runOnce is run without the nil-error demand: an idle pass is an outcome here.
func (r *titleRig) runOnce(t *testing.T) error {
	t.Helper()
	if _, err := r.db.Exec(`UPDATE work_queue SET next_attempt_at = '2000-01-01T00:00:00Z' WHERE id = ?`, r.id); err != nil {
		t.Fatal(err)
	}
	return r.w.RunOnce(context.Background())
}

// misses is every lane charged a miss in provider_outcomes, as "lane=n,...".
func (r *titleRig) misses(t *testing.T) string {
	t.Helper()
	rows, err := r.db.Query(`SELECT lane, misses FROM provider_outcomes WHERE misses > 0 ORDER BY lane`)
	if err != nil {
		t.Fatalf("read provider_outcomes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var lane string
		var n int
		if err := rows.Scan(&lane, &n); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s=%d", lane, n))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, ",")
}

// wantReleasedNoMiss checks the row an all-lanes-unavailable pass leaves
// behind: back to pending, no miss, no cooldown reason, no provider request,
// and no detector verdict stamped.
func wantReleasedNoMiss(t *testing.T, r *titleRig, err error) {
	t.Helper()
	if !errors.Is(err, errLanesUnavailable) {
		t.Errorf("RunOnce = %v; want errLanesUnavailable", err)
	}
	if got, want := r.cols(t, "status", "miss_count", "attempts", "last_error", "instrumental_result"), "pending|0|0||null"; got != want {
		t.Errorf("status|miss_count|attempts|last_error|instrumental_result = %q; want %q", got, want)
	}
	if got := r.misses(t); got != "" {
		t.Errorf("lanes charged a miss = %q; want none (no provider was asked)", got)
	}
	if got := r.row(t); !strings.HasSuffix(got, "attempts=0") {
		t.Errorf("row = %q; want no lane_attempts", got)
	}
	if r.fetches() != 0 {
		t.Errorf("provider requests = %d; want 0", r.fetches())
	}
}

// wantParkedNoMiss checks the row a detector "not instrumental" leaves behind
// when no provider could be asked: set aside into the future with the verdict
// stamped, no miss, no attempt, no provider request, and the pass not idled.
func wantParkedNoMiss(t *testing.T, r *titleRig, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("RunOnce = %v; want nil (the row is set aside and the pass goes on)", err)
	}
	if got, want := r.cols(t, "status", "miss_count", "attempts", "last_error", "instrumental_result"), "pending|0|0||0"; got != want {
		t.Errorf("status|miss_count|attempts|last_error|instrumental_result = %q; want %q", got, want)
	}
	if got := r.misses(t); got != "" {
		t.Errorf("lanes charged a miss = %q; want none (no provider was asked)", got)
	}
	if got := r.row(t); !strings.HasSuffix(got, "attempts=0") {
		t.Errorf("row = %q; want no lane_attempts", got)
	}
	if r.fetches() != 0 {
		t.Errorf("provider requests = %d; want 0", r.fetches())
	}
	if !r.waiting(t, r.id) {
		t.Errorf("next_attempt_at = %q; want a time ahead (the row waits for a provider)", r.cols(t, "next_attempt_at"))
	}
}

// waiting reports whether row id is held out of the ready set by a
// next_attempt_at still ahead.
func (r *titleRig) waiting(t *testing.T, id int64) bool {
	t.Helper()
	var at string
	if err := r.db.QueryRow(`SELECT next_attempt_at FROM work_queue WHERE id = ?`, id).Scan(&at); err != nil {
		t.Fatalf("read next_attempt_at: %v", err)
	}
	when, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t.Fatalf("parse next_attempt_at %q: %v", at, err)
	}
	return when.After(time.Now())
}

var noAskTrack = models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor", AlbumName: "Paper Kites"}

// Acceptance 1: every provider breaker open, the detector says not
// instrumental. The row is set aside until a provider may be probed again. The
// parallel + demoted cell never dequeues (the detector is asked only after a
// provider), so it has no verdict to stamp and idles.
func TestNoProviderAsked_DetectorNegativeIsNotAMiss(t *testing.T) {
	for _, c := range noAskCells {
		t.Run(c.String(), func(t *testing.T) {
			r, det := newNoAskRig(t, c, noAskTrack, false)
			r.tripProviders(t, 2)
			err := r.runOnce(t)
			stamp, calls := "0", 1
			if c.staged() {
				stamp, calls = "null", 0
				wantReleasedNoMiss(t, r, err)
			} else {
				wantParkedNoMiss(t, r, err)
			}
			if len(det.calls) != calls {
				t.Errorf("detector calls = %d; want %d", len(det.calls), calls)
			}
			if got := r.cols(t, "detector_version"); (got == "9.9.9") != (stamp == "0") {
				t.Errorf("detector_version = %q with instrumental_result %s", got, stamp)
			}
		})
	}
}

// Acceptance 2: every provider breaker open, the detector says instrumental.
// It settles the row with no provider request, wherever it may answer first.
func TestNoProviderAsked_DetectorPositiveStillSettles(t *testing.T) {
	for _, c := range noAskCells {
		t.Run(c.String(), func(t *testing.T) {
			r, _ := newNoAskRig(t, c, noAskTrack, true)
			r.tripProviders(t, 2)
			err := r.runOnce(t)
			if c.staged() {
				wantReleasedNoMiss(t, r, err)
				return
			}
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if got, want := r.cols(t, "status", "provider_lane", "instrumental_result", "miss_count"), "done|detector|1|0"; got != want {
				t.Errorf("status|provider_lane|instrumental_result|miss_count = %q; want %q", got, want)
			}
			if r.fetches() != 0 || r.misses(t) != "" {
				t.Errorf("provider requests = %d, misses = %q; want 0 and none", r.fetches(), r.misses(t))
			}
		})
	}
}

// Acceptance 3 and 4: a provider that WAS asked and missed is a miss for the
// row, and each lane that ran is charged one. A lane skipped on an open
// breaker is charged nothing.
func TestNoProviderAsked_AskedProviderStillChargesAMiss(t *testing.T) {
	for _, tc := range []struct {
		name    string
		open    int
		fetches int
		misses  string
	}{
		{"every provider asked", 0, 2, "detector=1,musixmatch=1,petitlyrics=1"},
		{"one provider open", 1, 1, "detector=1,petitlyrics=1"},
	} {
		for _, c := range noAskCells {
			t.Run(tc.name+"/"+c.String(), func(t *testing.T) {
				r, det := newNoAskRig(t, c, noAskTrack, false)
				r.tripProviders(t, tc.open)
				if err := r.runOnce(t); err != nil {
					t.Fatalf("RunOnce: %v", err)
				}
				if got, want := r.cols(t, "status", "miss_count", "instrumental_result"), "deferred|1|0"; got != want {
					t.Errorf("status|miss_count|instrumental_result = %q; want %q (a provider was asked and missed)", got, want)
				}
				if got := r.misses(t); got != tc.misses {
					t.Errorf("lanes charged a miss = %q; want %q", got, tc.misses)
				}
				if r.fetches() != tc.fetches || len(det.calls) != 1 {
					t.Errorf("provider requests = %d, detector calls = %d; want %d, 1", r.fetches(), len(det.calls), tc.fetches)
				}
			})
		}
	}
}

// Acceptance 5: no detector lane. The worker idles without claiming the row.
func TestNoProviderAsked_NoDetectorIdles(t *testing.T) {
	for _, mode := range []string{orchestrator.ModeOrdered, orchestrator.ModeParallel} {
		t.Run(mode, func(t *testing.T) {
			r := newTitleRig(t, noAskTrack)
			r.w.SetProvidersMode(mode)
			r.tripProviders(t, 2)
			wantReleasedNoMiss(t, r, r.runOnce(t))
		})
	}
}

// The Weak-title early ask (#1471) dials the detector before the dispatch. Its
// "no" with every provider breaker open is no miss either: it is stamped and
// the row is set aside.
func TestNoProviderAsked_WeakTitleEarlyAsk(t *testing.T) {
	for _, c := range noAskCells {
		t.Run(c.String(), func(t *testing.T) {
			r, det := newNoAskRig(t, c, weakTrack, false)
			r.w.SetTitleRuleWeak(true)
			r.tripProviders(t, 2)
			err := r.runOnce(t)
			calls := 1
			if c.staged() {
				calls = 0
				wantReleasedNoMiss(t, r, err)
			} else {
				wantParkedNoMiss(t, r, err)
			}
			if len(det.calls) != calls {
				t.Errorf("detector calls = %d; want %d", len(det.calls), calls)
			}
		})
	}
}

// pathDetector answers per audio path: a path in instrumental is instrumental,
// any other is not. calls counts the asks per path.
type pathDetector struct {
	instrumental map[string]bool
	calls        map[string]int
}

func (d *pathDetector) Detect(_ context.Context, audioPath string) (detector.Result, error) {
	d.calls[audioPath]++
	return detector.Result{Instrumental: d.instrumental[audioPath], Version: "9.9.9", Reusable: true}, nil
}

// noAskHeads are the head rows a healthy detector cannot settle: one it calls
// not instrumental (asks 1), and two it is never asked about (asks 0).
var noAskHeads = []struct {
	name, setup string
	asks        int
}{
	{"sung", "", 1},
	{"detection off", `UPDATE work_queue SET detect_instrumental = 0 WHERE id = ?`, 0},
	{"no source path", `UPDATE work_queue SET source_path = '' WHERE id = ?`, 0},
}

// A row at the head of the queue that the detector cannot settle must not keep
// it from an instrumental queued behind it while every provider is down
// (#1502): the head row is set aside with no miss and the pass goes on, whether
// the detector called it not instrumental or was never asked about it. The
// parallel + demoted cell idles before any dequeue, so neither row moves.
func TestNoProviderAsked_HeadRowDoesNotBlockInstrumentalBehindIt(t *testing.T) {
	for _, h := range noAskHeads {
		for _, c := range noAskCells {
			t.Run(h.name+"/"+c.String(), func(t *testing.T) { headRowDoesNotBlock(t, c, h.setup, h.asks) })
		}
	}
}

func headRowDoesNotBlock(t *testing.T, c noAskCell, setup string, asks int) {
	r, _ := newNoAskRig(t, c, noAskTrack, false)
	if setup != "" {
		if _, err := r.db.Exec(setup, r.id); err != nil {
			t.Fatalf("set up the head row: %v", err)
		}
	}
	sung := filepath.Join(r.dir, "track.flac")
	quiet := filepath.Join(r.dir, "quiet.flac")
	behind, err := r.q.Enqueue(context.Background(), models.Inputs{
		Track:  models.Track{ArtistName: "Tin Lantern Band", TrackName: "Salt Meridian", AlbumName: "Paper Kites"},
		Outdir: r.dir, Filename: "quiet.lrc", SourcePath: quiet,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue the row behind: %v", err)
	}
	det := &pathDetector{instrumental: map[string]bool{quiet: true}, calls: map[string]int{}}
	r.w.EnableAudioDetector(det)
	r.tripProviders(t, 2)

	// Drain as the run loop does: until a pass idles.
	var last error
	passes := 0
	for ; passes < 6; passes++ {
		if last = r.w.RunOnce(context.Background()); last != nil {
			break
		}
	}
	status := func(id int64) string {
		var got string
		if err := r.db.QueryRow(`SELECT status || '|' || miss_count || '|' || attempts || '|' || COALESCE(provider_lane, '')
			FROM work_queue WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read row %d: %v", id, err)
		}
		return got
	}
	if c.staged() {
		if !errors.Is(last, errLanesUnavailable) || passes != 0 {
			t.Errorf("drain ended with %v after %d passes; want errLanesUnavailable at once", last, passes)
		}
		if got := status(r.id) + " " + status(behind.ID); got != "pending|0|0| pending|0|0|" {
			t.Errorf("rows = %q; want both untouched", got)
		}
		if len(det.calls) != 0 {
			t.Errorf("detector calls = %v; want none", det.calls)
		}
		return
	}
	if !errors.Is(last, errQueueEmpty) || passes != 2 {
		t.Errorf("drain ended with %v after %d passes; want errQueueEmpty after 2 (one per row)", last, passes)
	}
	if got, want := status(behind.ID), "done|0|0|detector"; got != want {
		t.Errorf("instrumental row behind = %q; want %q (the detector must reach it)", got, want)
	}
	if got, want := status(r.id), "pending|0|0|"; got != want {
		t.Errorf("head row = %q; want %q (no miss, no attempt, not done)", got, want)
	}
	if got, want := r.cols(t, "instrumental_result"), map[int]string{0: "null", 1: "0"}[asks]; got != want {
		t.Errorf("head row instrumental_result = %q; want %q (a stamp only for a real verdict)", got, want)
	}
	if !r.waiting(t, r.id) {
		t.Errorf("head row next_attempt_at = %q; want a time ahead", r.cols(t, "next_attempt_at"))
	}
	if got := r.misses(t); got != "" || r.fetches() != 0 {
		t.Errorf("lanes charged a miss = %q, provider requests = %d; want none and 0", got, r.fetches())
	}
	// The wait holds: further polls neither claim the sung row nor re-ask
	// the detector about it.
	for i := 0; i < 3; i++ {
		if err := r.w.RunOnce(context.Background()); !errors.Is(err, errQueueEmpty) {
			t.Errorf("poll %d during the wait = %v; want errQueueEmpty", i, err)
		}
	}
	if det.calls[sung] != asks || det.calls[quiet] != 1 {
		t.Errorf("detector calls = %v; want %d for the head row and 1 behind it while the wait holds", det.calls, asks)
	}
	// Once the wait has passed the row is claimed again, and with the
	// providers still down it is set aside again, still with no miss.
	if err := r.runOnce(t); err != nil {
		t.Errorf("RunOnce after the wait = %v; want nil", err)
	}
	if det.calls[sung] != 2*asks {
		t.Errorf("detector calls for the head row after the wait = %d; want %d", det.calls[sung], 2*asks)
	}
	if got, want := status(r.id), "pending|0|0|"; got != want || !r.waiting(t, r.id) {
		t.Errorf("head row after the wait = %q, waiting %v; want %q and waiting", got, r.waiting(t, r.id), want)
	}
}

// The memo's per-row reset (#1502): what the detector did for row A must not
// decide row B, which it is never asked about. After A's verdict, B must carry
// no stamp; after A's failure (the pass idles, A is then moved out of the way),
// B must still be set aside, not idled on A's error.
func TestNoProviderAsked_EachRowIsJudgedAlone(t *testing.T) {
	for _, first := range []string{"verdict", "failure"} {
		for _, c := range noAskCells[:2] {
			t.Run(first+"/"+c.String(), func(t *testing.T) {
				r, det := newNoAskRig(t, c, noAskTrack, false)
				off := false
				b, err := r.q.Enqueue(context.Background(), models.Inputs{
					Track:  models.Track{ArtistName: "Tin Lantern Band", TrackName: "Salt Meridian", AlbumName: "Paper Kites"},
					Outdir: r.dir, Filename: "other.lrc", SourcePath: filepath.Join(r.dir, "other.flac"), DetectInstrumental: &off,
				}, queue.PriorityScan)
				if err != nil {
					t.Fatalf("enqueue row B: %v", err)
				}
				r.tripProviders(t, 2)
				if first == "failure" {
					det.err = fmt.Errorf("dial: %w", detector.ErrClassifierNotReady)
				}
				err = r.w.RunOnce(context.Background())
				if first == "failure" {
					if !errors.Is(err, errLanesUnavailable) {
						t.Fatalf("row A, detector failed: RunOnce = %v; want errLanesUnavailable", err)
					}
					if _, err := r.db.Exec(`UPDATE work_queue SET next_attempt_at = '2999-01-01T00:00:00Z' WHERE id = ?`, r.id); err != nil {
						t.Fatal(err)
					}
				} else if err != nil || r.cols(t, "instrumental_result") != "0" {
					t.Fatalf("row A, verdict: RunOnce = %v, instrumental_result = %s; want nil and 0", err, r.cols(t, "instrumental_result"))
				}
				if err := r.w.RunOnce(context.Background()); err != nil {
					t.Errorf("row B: RunOnce = %v; want nil (B was not asked about, so A's outcome must not idle the pass)", err)
				}
				var got string
				if err := r.db.QueryRow(`SELECT status || '|' || miss_count || '|' || COALESCE(CAST(instrumental_result AS TEXT), 'null') || '|' ||
					COALESCE(detector_version, 'null') FROM work_queue WHERE id = ?`, b.ID).Scan(&got); err != nil {
					t.Fatalf("read row B: %v", err)
				}
				if want := "pending|0|null|null"; got != want {
					t.Errorf("row B status|miss_count|instrumental_result|detector_version = %q; want %q (no stamp from row A)", got, want)
				}
				if !r.waiting(t, b.ID) || len(det.calls) != 1 {
					t.Errorf("row B waiting = %v, detector calls = %d; want set aside and 1 call (row A only)", r.waiting(t, b.ID), len(det.calls))
				}
			})
		}
	}
}

// trippingDetector answers "not instrumental" and opens its own lane's breaker
// as it does, so the dispatch that follows the Weak-title early ask finds every
// lane open and consults none.
type trippingDetector struct{ trip func() }

func (d trippingDetector) Detect(context.Context, string) (detector.Result, error) {
	d.trip()
	return detector.Result{Version: "9.9.9", Reusable: true}, nil
}

// A dispatch in which no lane ran at all idles, even though the detector did
// not fail on the row: there is no healthy lane left to reach the rows behind.
func TestNoProviderAsked_NoLaneConsultedIdles(t *testing.T) {
	for _, c := range noAskCells[:2] {
		t.Run(c.String(), func(t *testing.T) {
			r, _ := newNoAskRig(t, c, weakTrack, false)
			r.w.SetTitleRuleWeak(true)
			r.w.EnableAudioDetector(trippingDetector{trip: func() { r.w.detectorLane.Breaker().Trip() }})
			r.tripProviders(t, 2)
			if err := r.runOnce(t); !errors.Is(err, errLanesUnavailable) {
				t.Errorf("RunOnce = %v; want errLanesUnavailable (no lane was consulted)", err)
			}
			if got := r.cols(t, "status", "miss_count"); got != "pending|0" || r.waiting(t, r.id) {
				t.Errorf("status|miss_count = %q, waiting %v; want pending|0 and no wait", got, r.waiting(t, r.id))
			}
		})
	}
}

// A detector that failed on the row, with every provider breaker open, is a
// lane that is down: the row is released with no wait and the pass idles,
// unlike a row a healthy detector could not settle, which is set aside.
func TestNoProviderAsked_DetectorWithoutAVerdictIdles(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"outage", errors.New("classifier returned 500")},
		{"not ready", fmt.Errorf("dial: %w", detector.ErrClassifierNotReady)},
	} {
		for _, c := range noAskCells {
			t.Run(tc.name+"/"+c.String(), func(t *testing.T) {
				r, det := newNoAskRig(t, c, noAskTrack, false)
				det.err = tc.err
				r.tripProviders(t, 2)
				err := r.runOnce(t)
				wantReleasedNoMiss(t, r, err)
				if r.waiting(t, r.id) {
					t.Errorf("next_attempt_at = %q; want no wait (no lane made progress)", r.cols(t, "next_attempt_at"))
				}
				calls := 1
				if c.staged() {
					calls = 0
				}
				if len(det.calls) != calls {
					t.Errorf("detector calls = %d; want %d", len(det.calls), calls)
				}
			})
		}
	}
}
