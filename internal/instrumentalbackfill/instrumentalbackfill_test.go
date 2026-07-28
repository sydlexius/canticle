package instrumentalbackfill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/detectorbackfill"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// --- fakes ---------------------------------------------------------------

type fakeStore struct {
	items []queue.WorkItem
	total int

	countErr error
	listErr  error
	stampErr error

	settleErr error
	// settleOutcome is what SettleInstrumental reports. Defaults to queue.Settled;
	// set it to SettleClaimed / SettleAlreadyInstrumental / SettleRowGone to stage
	// the races those outcomes represent.
	settleOutcome queue.SettleOutcome
	// stampClaimed makes StampUnclassifiedMiss report stamped=false.
	stampClaimed bool
	// order, when set, records the mutation sequence so a test can assert ORDERING
	// rather than mere callback execution.
	order *[]string

	lastOpts    queue.ListUnclassifiedOptions
	stamped     map[int64]int
	settled     []int64
	stampCalls  int
	settleCalls int
	// laneAttempts is the LAST attempt recorded per queue id, mirroring the real
	// table's UNIQUE(queue_id, lane) upsert.
	laneAttempts     map[int64]models.LaneAttempt
	laneAttemptCalls int
	laneAttemptErr   error
	// t reports a wrong-ownership call. The fake asserts the argument rather than
	// ignoring it, because ownership is the one parameter that silently no-ops in
	// production when it is wrong.
	t *testing.T
}

func newFakeStore(t *testing.T, items ...queue.WorkItem) *fakeStore {
	t.Helper()
	return &fakeStore{
		items:         items,
		total:         len(items),
		stamped:       map[int64]int{},
		settleOutcome: queue.Settled,
		t:             t,
	}
}

func (s *fakeStore) CountUnclassified(_ context.Context, _ *int64, _ bool) (int, error) {
	return s.total, s.countErr
}

func (s *fakeStore) ListUnclassified(_ context.Context, opts queue.ListUnclassifiedOptions) ([]queue.WorkItem, error) {
	s.lastOpts = opts
	if s.listErr != nil {
		return nil, s.listErr
	}
	items := s.items
	if opts.Limit > 0 && len(items) > opts.Limit {
		items = items[:opts.Limit]
	}
	return items, nil
}

func (s *fakeStore) SettleInstrumental(_ context.Context, id int64, _ queue.InstrumentalTelemetry, owner queue.RowOwnership) (queue.SettleOutcome, error) {
	// The backfill does not own its rows; passing OwnedByWorker would guard on the
	// wrong status and settle nothing in production, so assert it here.
	if owner != queue.OwnedByBackfill {
		s.t.Errorf("SettleInstrumental owner = %v; want OwnedByBackfill", owner)
	}
	s.settleCalls++
	if s.order != nil {
		*s.order = append(*s.order, "settle")
	}
	if s.settleErr != nil {
		return queue.SettleFailed, s.settleErr
	}
	if s.settleOutcome != queue.Settled {
		return s.settleOutcome, nil
	}
	s.stamped[id] = 1
	s.settled = append(s.settled, id)
	return queue.Settled, nil
}

func (s *fakeStore) StampUnclassifiedMiss(_ context.Context, id int64, _ queue.InstrumentalTelemetry) (bool, error) {
	s.stampCalls++
	if s.stampErr != nil {
		return false, s.stampErr
	}
	if s.stampClaimed {
		return false, nil
	}
	s.stamped[id] = 0
	return true, nil
}

// RecordLaneAttempts records the attempts the backfill reports, keyed by queue
// id, so a test can assert BOTH that an attempt was recorded and which verdict
// it carried. It models the real UNIQUE(queue_id, lane) upsert by overwriting
// rather than appending: a fake that appended would hide a double-record bug the
// real schema silently collapses.
func (s *fakeStore) RecordLaneAttempts(_ context.Context, queueID int64, attempts []models.LaneAttempt) error {
	s.laneAttemptCalls++
	if s.laneAttemptErr != nil {
		return s.laneAttemptErr
	}
	if s.laneAttempts == nil {
		s.laneAttempts = map[int64]models.LaneAttempt{}
	}
	for _, a := range attempts {
		s.laneAttempts[queueID] = a
	}
	return nil
}

type fakeDetector struct {
	res detector.Result
	err error
}

func (d fakeDetector) Detect(_ context.Context, _ string) (detector.Result, error) {
	return d.res, d.err
}

// fakeWriter records what a real lyrics.Writer would ACTUALLY put on disk. It
// derives the sidecar name with lyrics.SidecarName -- the same call the real
// writer makes -- instead of echoing the filename it was handed.
//
// An earlier version appended outdir+"/"+filename, which is exactly the bug the
// production code had: it ignored that an instrumental marker is unsynced and
// lands as .txt, not the enqueued .lrc. Because the fake shared the bug, every
// test agreed with the broken code and none could see it. A fake that does not
// faithfully model its seam validates nothing.
type fakeWriter struct {
	err     error
	written []string
	// order, when set, records "marker" so a test can assert the real sequence.
	order *[]string
}

func (w *fakeWriter) WriteLRC(song models.Song, filename, outdir string) error {
	if w.err != nil {
		return w.err
	}
	name, err := lyrics.SidecarName(song.Track.ArtistName, song.Track.TrackName, filename, false)
	if err != nil {
		return err
	}
	w.written = append(w.written, filepath.Join(outdir, name))
	if w.order != nil {
		*w.order = append(*w.order, "marker")
	}
	return nil
}

func item(id int64, src string) queue.WorkItem {
	return queue.WorkItem{ID: id, Inputs: models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:     "out",
		Filename:   "song.lrc",
		SourcePath: src,
	}}
}

func instrumentalVerdict() detector.Result {
	return detector.Result{Instrumental: true, Confidence: 0.95, Version: "v1"}
}

// --- tests ---------------------------------------------------------------

func TestRun_SettlesInstrumentalRowBackupFirst(t *testing.T) {
	var order []string
	store := newFakeStore(t, item(1, "/music/a.flac"))
	store.order = &order
	w := &fakeWriter{order: &order}

	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, w).Run(context.Background(), Options{
		GlobalDetectDefault: true,
		Report: func(Change) error {
			order = append(order, "backup")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Instrumental != 1 || res.MarkersWritten != 1 || res.RowsSettled != 1 {
		t.Fatalf("res = %+v; want instrumental/markers/settled all 1", res)
	}
	if store.stamped[1] != 1 {
		t.Errorf("stamped = %v; want 1", store.stamped[1])
	}

	// Assert the ACTUAL sequence, not merely that the backup ran. An earlier
	// version only recorded "backup", so it passed even if the report fired last --
	// which is precisely the ordering the backup-first contract exists to forbid.
	want := []string{"backup", "marker", "settle"}
	if len(order) != len(want) {
		t.Fatalf("order = %v; want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v; want %v (the restorable record must exist before the change it describes)", order, want)
		}
	}
}

// TestRun_OutcomeReportsAppliedAfterSettle verifies the realized-outcome callback
// fires applied AFTER the settle, so the backup trail records what landed and not
// just intent (#515).
func TestRun_OutcomeReportsAppliedAfterSettle(t *testing.T) {
	var order []string
	store := newFakeStore(t, item(1, "/music/a.flac"))
	store.order = &order
	w := &fakeWriter{order: &order}

	var outcomes []Outcome
	_, err := New(store, fakeDetector{res: instrumentalVerdict()}, w).Run(context.Background(), Options{
		GlobalDetectDefault: true,
		Report:              func(Change) error { order = append(order, "backup"); return nil },
		Outcome: func(o Outcome) error {
			order = append(order, "outcome")
			outcomes = append(outcomes, o)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].QueueID != 1 || outcomes[0].Status != OutcomeApplied || outcomes[0].Ambiguous {
		t.Fatalf("outcomes=%+v; want one {id=1 applied} record", outcomes)
	}
	// Outcome must come AFTER the settle -- it reports the realized result.
	want := []string{"backup", "marker", "settle", "outcome"}
	if len(order) != len(want) {
		t.Fatalf("order = %v; want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v; want %v", order, want)
		}
	}
}

// TestRun_OutcomeReportsSkippedOnClaim verifies a stamp-claimed negative verdict
// fires skipped (nothing landed), and TestRun_OutcomeReportsAmbiguous the
// ambiguous-settle failed case (#515).
func TestRun_OutcomeReportsSkippedOnClaim(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	store.stampClaimed = true // negative verdict, but a worker claimed the row

	var outcomes []Outcome
	_, err := New(store, fakeDetector{res: detector.Result{Instrumental: false, Version: "v1"}}, &fakeWriter{}).Run(context.Background(), Options{
		GlobalDetectDefault: true,
		Outcome:             func(o Outcome) error { outcomes = append(outcomes, o); return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].Status != OutcomeSkipped {
		t.Fatalf("outcomes=%+v; want one skipped record", outcomes)
	}
}

func TestRun_OutcomeReportsAmbiguousOnSettleError(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	store.settleErr = errors.New("commit failed")

	var outcomes []Outcome
	_, err := New(store, fakeDetector{res: instrumentalVerdict()}, &fakeWriter{}).Run(context.Background(), Options{
		GlobalDetectDefault: true,
		Outcome:             func(o Outcome) error { outcomes = append(outcomes, o); return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].Status != OutcomeFailed || !outcomes[0].Ambiguous {
		t.Fatalf("outcomes=%+v; want one {failed ambiguous} record", outcomes)
	}
}

// A Report failure must abort that row's mutation entirely: the whole point of
// backup-first is that a change never exists without its restorable record.
func TestRun_ReportFailureAbortsRowMutation(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	w := &fakeWriter{}

	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, w).Run(context.Background(), Options{
		GlobalDetectDefault: true,
		Report:              func(Change) error { return errors.New("disk full") },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errors != 1 || res.RowsSettled != 0 {
		t.Fatalf("res = %+v; want errors=1 settled=0", res)
	}
	if len(w.written) != 0 {
		t.Errorf("wrote a marker despite a failed backup: %v", w.written)
	}
	if store.stampCalls != 0 {
		t.Errorf("stamped a verdict despite a failed backup (%d calls)", store.stampCalls)
	}
}

// A failed marker write must leave the row unstamped: a row claiming
// instrumental with nothing on disk is worse than an unexamined row.
func TestRun_MarkerWriteFailureLeavesRowUnstamped(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	w := &fakeWriter{err: errors.New("read-only filesystem")}

	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, w).Run(context.Background(), Options{
		GlobalDetectDefault: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errors != 1 || res.RowsSettled != 0 || res.MarkersWritten != 0 {
		t.Fatalf("res = %+v; want errors=1 settled=0 markers=0", res)
	}
	if store.stampCalls != 0 {
		t.Errorf("stamped instrumental despite no marker on disk (%d calls)", store.stampCalls)
	}
	if store.settleCalls != 0 {
		t.Errorf("settled the row despite no marker on disk (%d calls)", store.settleCalls)
	}
}

func TestRun_NotInstrumentalStampsZeroAndDoesNotWrite(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	w := &fakeWriter{}

	res, err := New(store, fakeDetector{res: detector.Result{Instrumental: false, Version: "v1"}}, w).Run(
		context.Background(), Options{GlobalDetectDefault: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.NotInstrumental != 1 || res.RowsSettled != 0 {
		t.Fatalf("res = %+v; want not-instrumental=1 settled=0", res)
	}
	if len(w.written) != 0 {
		t.Errorf("a vocal track must never get a marker: %v", w.written)
	}
	if got, ok := store.stamped[1]; !ok || got != 0 {
		t.Errorf("stamped = %v (present=%v); want 0 so it is distinguishable from never-detected", got, ok)
	}
}

// TestRun_RecordsDetectorLaneAttemptForBothVerdicts pins the per-track hit-rate
// reporting the backfill owes.
//
// The backfill reaches its verdict through a different path than the worker and
// recorded NO lane_attempts at all, so the report's detector tile counted only
// worker-side detections. On a live install that tile sat frozen at a fixed
// numerator AND denominator while a backfill classified thousands of tracks --
// not merely undercounting, but looking broken.
//
// BOTH verdicts are asserted because the tile renders a hit RATE: recording only
// the instrumental settles would drive the detector toward a meaningless 100%,
// the same trap migration 029 calls out for the historical backfill. A test that
// checked only the positive path would pass against exactly that bug.
func TestRun_RecordsDetectorLaneAttemptForBothVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		instrumental bool
	}{
		{"instrumental settle records a hit", true},
		{"not-instrumental stamp records a miss", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(t, item(1, "/music/a.flac"))
			res, err := New(store, fakeDetector{res: detector.Result{Instrumental: tc.instrumental, Version: "v1"}}, &fakeWriter{}).Run(
				context.Background(), Options{GlobalDetectDefault: true})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Errors != 0 {
				t.Fatalf("res = %+v; want no errors", res)
			}
			got, ok := store.laneAttempts[1]
			if !ok {
				t.Fatal("no lane attempt recorded; the detector tile cannot see this row's classification and stays frozen while the backfill runs")
			}
			if got.Hit != tc.instrumental {
				t.Errorf("attempt Hit = %v; want %v. The tile renders a hit RATE, so a wrong or missing verdict skews it", got.Hit, tc.instrumental)
			}
			if got.Lane != detectorbackfill.LaneName {
				t.Errorf("attempt Lane = %q; want %q so it aggregates with the worker's detector attempts", got.Lane, detectorbackfill.LaneName)
			}
			if !got.Local {
				t.Error("attempt Local = false; the detector resolves a track with no outbound provider request")
			}
		})
	}
}

// TestRun_RecordsNoLaneAttemptWhenTheRowWasClaimed: a row a worker claimed
// mid-classification was not settled by this run, so crediting the detector with
// an attempt on it would report work that never landed.
func TestRun_RecordsNoLaneAttemptWhenTheRowWasClaimed(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	store.settleOutcome = queue.SettleClaimed

	if _, err := New(store, fakeDetector{res: detector.Result{Instrumental: true, Version: "v1"}}, &fakeWriter{}).Run(
		context.Background(), Options{GlobalDetectDefault: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := store.laneAttempts[1]; ok {
		t.Error("recorded a lane attempt for a row a worker claimed; the settle never applied, so the detector did not resolve this track")
	}
}

// TestRun_LaneAttemptFailureDoesNotFailTheRow: the verdict is already durably
// recorded by the time the attempt is reported, so losing the report must not
// turn a successful settle into an error.
func TestRun_LaneAttemptFailureDoesNotFailTheRow(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	store.laneAttemptErr = errors.New("db is busy")

	res, err := New(store, fakeDetector{res: detector.Result{Instrumental: true, Version: "v1"}}, &fakeWriter{}).Run(
		context.Background(), Options{GlobalDetectDefault: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RowsSettled != 1 || res.Errors != 0 {
		t.Errorf("res = %+v; want settled=1 errors=0: a reporting failure must not fail a row whose settle committed", res)
	}
}

func TestRun_DryRunPreviewsAndMutatesNothing(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	w := &fakeWriter{}
	var previewed []int64

	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, w).Run(context.Background(), Options{
		GlobalDetectDefault: true,
		DryRun:              true,
		Preview:             func(ch Change) { previewed = append(previewed, ch.QueueID) },
		Report:              func(Change) error { t.Fatal("dry run must not write a backup record"); return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Instrumental != 1 {
		t.Errorf("res.Instrumental = %d; want 1 (a dry run still classifies)", res.Instrumental)
	}
	if res.RowsSettled != 0 || len(w.written) != 0 || store.stampCalls != 0 {
		t.Errorf("dry run mutated something: settled=%d written=%v stamps=%d", res.RowsSettled, w.written, store.stampCalls)
	}
	if len(previewed) != 1 || previewed[0] != 1 {
		t.Errorf("previewed = %v; want [1]", previewed)
	}
}

func TestRun_HonorsPerItemOptOutOverGlobalDefault(t *testing.T) {
	optOut := false
	it := item(1, "/music/a.flac")
	it.DetectInstrumental = &optOut
	store := newFakeStore(t, it)
	w := &fakeWriter{}

	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, w).Run(context.Background(), Options{
		GlobalDetectDefault: true, // global says yes; the row says no and must win
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SkippedDetectOff != 1 || res.Checked != 0 {
		t.Fatalf("res = %+v; want detect-off=1 checked=0", res)
	}
	if len(w.written) != 0 {
		t.Errorf("opted-out row got a marker: %v", w.written)
	}
}

// A per-item opt-IN must survive a global default of off, so a library that
// enabled detection is still backfilled when the global switch is off.
func TestRun_PerItemOptInOverridesGlobalOff(t *testing.T) {
	optIn := true
	it := item(1, "/music/a.flac")
	it.DetectInstrumental = &optIn
	store := newFakeStore(t, it)

	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, &fakeWriter{}).Run(context.Background(), Options{
		GlobalDetectDefault: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Checked != 1 || res.Instrumental != 1 {
		t.Fatalf("res = %+v; want the row classified despite the global default being off", res)
	}
}

func TestRun_DetectorFailureIsNonFatalAndLeavesRowAlone(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"), item(2, "/music/b.flac"))
	w := &fakeWriter{}

	res, err := New(store, fakeDetector{err: errors.New("sidecar down")}, w).Run(context.Background(), Options{
		GlobalDetectDefault: true,
	})
	if err != nil {
		t.Fatalf("Run should not abort on a per-row detector failure: %v", err)
	}
	if res.Errors != 2 || res.Checked != 0 || res.RowsSettled != 0 {
		t.Fatalf("res = %+v; want errors=2 checked=0 settled=0", res)
	}
	if store.stampCalls != 0 {
		t.Errorf("stamped a verdict the detector never produced (%d calls)", store.stampCalls)
	}
}

func TestRun_SkipsRowWithNoSourcePath(t *testing.T) {
	store := newFakeStore(t, item(1, "   "))
	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, &fakeWriter{}).Run(
		context.Background(), Options{GlobalDetectDefault: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SkippedNoSource != 1 || res.Checked != 0 {
		t.Fatalf("res = %+v; want no-source=1 checked=0", res)
	}
}

// Result.Total must report the FULL backlog even when Limit caps the candidate
// set, so a capped run can say what it left behind rather than reading as full
// coverage.
func TestRun_LimitCapsCandidatesButTotalReportsBacklog(t *testing.T) {
	store := newFakeStore(t, item(1, "/a.flac"), item(2, "/b.flac"), item(3, "/c.flac"))
	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, &fakeWriter{}).Run(
		context.Background(), Options{GlobalDetectDefault: true, Limit: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Total != 3 {
		t.Errorf("Total = %d; want 3 (the whole backlog)", res.Total)
	}
	if res.Candidates != 1 || res.Checked != 1 {
		t.Errorf("res = %+v; want candidates=1 checked=1", res)
	}
	if store.lastOpts.Limit != 1 {
		t.Errorf("Limit was not passed to the store: %+v", store.lastOpts)
	}
}

// The miss path's stamp failure must be counted, not swallowed.
func TestRun_MissStampFailureIsCounted(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	store.stampErr = errors.New("db locked")

	res, err := New(store, fakeDetector{res: detector.Result{Instrumental: false, Version: "v1"}}, &fakeWriter{}).Run(
		context.Background(), Options{GlobalDetectDefault: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errors != 1 || res.NotInstrumental != 1 {
		t.Fatalf("res = %+v; want errors=1 not-instrumental=1", res)
	}
}

func TestRun_SettleFailureCountsErrorAndDoesNotClaimSuccess(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	store.settleErr = errors.New("row owned by a worker")

	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, &fakeWriter{}).Run(
		context.Background(), Options{GlobalDetectDefault: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errors != 1 || res.RowsSettled != 0 {
		t.Fatalf("res = %+v; want errors=1 settled=0", res)
	}
}

func TestRun_CountFailureAborts(t *testing.T) {
	store := newFakeStore(t)
	store.countErr = errors.New("db gone")
	if _, err := New(store, fakeDetector{}, &fakeWriter{}).Run(context.Background(), Options{}); err == nil {
		t.Fatal("Run must abort when the backlog cannot be enumerated")
	}
}

func TestRun_ListFailureAborts(t *testing.T) {
	store := newFakeStore(t)
	store.listErr = errors.New("db gone")
	if _, err := New(store, fakeDetector{}, &fakeWriter{}).Run(context.Background(), Options{}); err == nil {
		t.Fatal("Run must abort when candidates cannot be listed")
	}
}

func TestRun_CancelledContextStopsWithoutMutating(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, &fakeWriter{}).Run(ctx, Options{
		GlobalDetectDefault: true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
	if res.RowsSettled != 0 || store.stampCalls != 0 {
		t.Errorf("mutated after cancellation: settled=%d stamps=%d", res.RowsSettled, store.stampCalls)
	}
}

// TestMarkerPaths_NamesTheFileTheWriterActuallyWrites is the regression guard for
// the backup contract. An instrumental marker is UNSYNCED, so lyrics.SidecarName
// rewrites an enqueued "song.lrc" to "song.txt". Naming the raw enqueued filename
// produced a backup record pointing at a path that never existed -- a restorable
// record that cannot restore. This asserts MarkerPaths agrees with what a real
// write puts on disk, which is the only claim the backup makes.
func TestMarkerPaths_NamesTheFileTheWriterActuallyWrites(t *testing.T) {
	it := item(1, "/music/a.flac")

	claimed := MarkerPaths(it.Inputs)

	w := &fakeWriter{}
	if _, err := (&Backfiller{w: w}).writeMarkers(it); err != nil {
		t.Fatalf("writeMarkers: %v", err)
	}

	if len(claimed) != len(w.written) {
		t.Fatalf("MarkerPaths = %v but the writer wrote %v", claimed, w.written)
	}
	for i := range claimed {
		if claimed[i] != w.written[i] {
			t.Errorf("backup record claims %q but the writer wrote %q; a backup naming a nonexistent path cannot restore anything",
				claimed[i], w.written[i])
		}
	}
	if filepath.Ext(claimed[0]) != ".txt" {
		t.Errorf("MarkerPaths = %q; an instrumental marker is unsynced and must land as .txt, not the enqueued .lrc", claimed[0])
	}
}

// TestRun_WorkerClaimedRowLeavesNoOrphanMarker: the backfill does not own its
// rows, so a serve-mode worker can claim one while the detector runs. When the
// guarded settle then reports it wrote nothing, the marker this run put on disk
// must be taken back -- a sidecar the database has no record of is exactly the
// inconsistency the guard exists to prevent.
func TestRun_WorkerClaimedRowLeavesNoOrphanMarker(t *testing.T) {
	dir := t.TempDir()
	it := queue.WorkItem{ID: 1, Inputs: models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:     dir,
		Filename:   "song.lrc",
		SourcePath: "/music/a.flac",
	}}
	store := newFakeStore(t, it)
	store.settleOutcome = queue.SettleClaimed // a worker took the row mid-classification

	// A real writer, so a real file lands on disk and must really be removed.
	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, lyrics.NewLRCWriter()).Run(
		context.Background(), Options{GlobalDetectDefault: true, Report: func(Change) error { return nil }})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.SkippedClaimed != 1 {
		t.Errorf("res = %+v; want SkippedClaimed=1", res)
	}
	if res.RowsSettled != 0 {
		t.Errorf("RowsSettled = %d; want 0 (the settle wrote nothing)", res.RowsSettled)
	}
	for _, p := range MarkerPaths(it.Inputs) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("orphan marker survived at %s: the DB has no record of it (stat err=%v)", p, err)
		}
	}
	if res.MarkersWritten != 0 {
		t.Errorf("MarkersWritten = %d; want 0 after the marker was taken back", res.MarkersWritten)
	}
}

// TestRun_PeerSettledRowKeepsItsMarker is the data-loss guard. Zero affected rows
// only proves the row is no longer deferred -- it does NOT prove a worker took it.
// A PEER BACKFILL may have settled it first with the identical verdict, in which
// case the marker on disk is correct and deleting it would destroy a valid result
// over a race we lost harmlessly.
func TestRun_PeerSettledRowKeepsItsMarker(t *testing.T) {
	dir := t.TempDir()
	it := queue.WorkItem{ID: 1, Inputs: models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:     dir,
		Filename:   "song.lrc",
		SourcePath: "/music/a.flac",
	}}
	store := newFakeStore(t, it)
	store.settleOutcome = queue.SettleAlreadyInstrumental // a peer got there first

	var outcomes []Outcome
	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, lyrics.NewLRCWriter()).Run(
		context.Background(), Options{
			GlobalDetectDefault: true,
			Report:              func(Change) error { return nil },
			Outcome:             func(o Outcome) error { outcomes = append(outcomes, o); return nil },
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.SkippedAlreadySettled != 1 {
		t.Errorf("res = %+v; want SkippedAlreadySettled=1", res)
	}
	// The peer-settled row reports OutcomeApplied (verdict on disk), the documented
	// #515 contract -- see the engine comment on this arm. Pin it here.
	if len(outcomes) != 1 || outcomes[0].Status != OutcomeApplied || outcomes[0].Ambiguous {
		t.Fatalf("outcomes=%+v; want one applied record", outcomes)
	}
	for _, p := range MarkerPaths(it.Inputs) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("peer-settled marker was DELETED at %s: the peer's result is valid and this run must not destroy it (%v)", p, err)
		}
	}
	if res.MarkersWritten != 1 {
		t.Errorf("MarkersWritten = %d; want 1 (the marker stands, it is simply the peer's)", res.MarkersWritten)
	}
	// The peer recorded its OWN attempt when it settled, so recording one here
	// would attribute the same track to the detector twice. The engine skips it
	// deliberately (only queue.Settled records); this pins that, since a double
	// count is invisible in the report -- it just makes the rate quietly wrong.
	if _, ok := store.laneAttempts[1]; ok {
		t.Error("recorded a lane attempt for a PEER-settled row; the peer already recorded its own, " +
			"so this double-attributes the track and skews the per-track hit rate")
	}
}

// A settle ERROR is ambiguous -- the failure may have come from Commit itself, so
// the row may or may not be settled. Deleting the marker could destroy a committed
// result, so it must survive; an orphan is recoverable, a deleted valid marker is
// not.
func TestRun_AmbiguousSettleErrorKeepsMarker(t *testing.T) {
	dir := t.TempDir()
	it := queue.WorkItem{ID: 1, Inputs: models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:     dir,
		Filename:   "song.lrc",
		SourcePath: "/music/a.flac",
	}}
	store := newFakeStore(t, it)
	store.settleErr = errors.New("commit failed: outcome unknown")

	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, lyrics.NewLRCWriter()).Run(
		context.Background(), Options{GlobalDetectDefault: true, Report: func(Change) error { return nil }})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errors != 1 || res.RowsSettled != 0 {
		t.Errorf("res = %+v; want errors=1 settled=0", res)
	}
	for _, p := range MarkerPaths(it.Inputs) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("marker deleted on an AMBIGUOUS settle error at %s; the commit may have landed (%v)", p, err)
		}
	}
}

// Verdict counts are a separate axis from mutation outcomes: a worker claim does
// not change what the detector heard. Instrumental + NotInstrumental must always
// equal Checked, or the summary underreports.
func TestRun_VerdictCountsSurviveAWorkerClaim(t *testing.T) {
	dir := t.TempDir()
	it := queue.WorkItem{ID: 1, Inputs: models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:     dir,
		Filename:   "song.lrc",
		SourcePath: "/music/a.flac",
	}}
	store := newFakeStore(t, it)
	store.settleOutcome = queue.SettleClaimed

	res, err := New(store, fakeDetector{res: instrumentalVerdict()}, lyrics.NewLRCWriter()).Run(
		context.Background(), Options{GlobalDetectDefault: true, Report: func(Change) error { return nil }})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Checked != res.Instrumental+res.NotInstrumental {
		t.Errorf("res = %+v; Checked must always equal Instrumental+NotInstrumental -- the detector's verdict stands regardless of what happened to the row", res)
	}
	if res.Instrumental != 1 {
		t.Errorf("Instrumental = %d; want 1 (the detector said instrumental; the claim did not change that)", res.Instrumental)
	}
	if res.SkippedClaimed != 1 || res.RowsSettled != 0 {
		t.Errorf("res = %+v; want claimed=1 settled=0", res)
	}
}

// A negative verdict is a mutation too -- it stamps instrumental_result=0, which
// retires the row from every future backfill. It must be backed up first.
func TestRun_NegativeVerdictIsBackedUpBeforeStamping(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))
	var order []string
	store.order = &order

	res, err := New(store, fakeDetector{res: detector.Result{Instrumental: false, Version: "v1"}}, &fakeWriter{}).Run(
		context.Background(), Options{
			GlobalDetectDefault: true,
			Report: func(ch Change) error {
				if ch.Instrumental {
					t.Error("Change.Instrumental = true for a negative verdict")
				}
				order = append(order, "backup")
				return nil
			},
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RowsStamped != 1 {
		t.Errorf("res = %+v; want RowsStamped=1", res)
	}
	if len(order) == 0 || order[0] != "backup" {
		t.Fatalf("order = %v; a negative stamp retires the row from future backfills and must be recorded BEFORE it lands", order)
	}
}

// A Report failure on the negative path must abort the stamp: no record, no change.
func TestRun_NegativeVerdictReportFailureAbortsStamp(t *testing.T) {
	store := newFakeStore(t, item(1, "/music/a.flac"))

	res, err := New(store, fakeDetector{res: detector.Result{Instrumental: false, Version: "v1"}}, &fakeWriter{}).Run(
		context.Background(), Options{
			GlobalDetectDefault: true,
			Report:              func(Change) error { return errors.New("disk full") },
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errors != 1 || res.RowsStamped != 0 {
		t.Fatalf("res = %+v; want errors=1 stamped=0", res)
	}
	if store.stampCalls != 0 {
		t.Errorf("stamped despite a failed backup (%d calls)", store.stampCalls)
	}
}
