package instrumentalrecalib

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	dbpkg "github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

type fakeWriter struct {
	calls int
	err   error
}

func (f *fakeWriter) WriteLRC(_ models.Song, _ string, _ string) error {
	f.calls++
	return f.err
}

// fsWriter writes a REAL sidecar file at the exact path writeMarkers will later
// try to roll back, and records it, so a test can assert on-disk state (the
// marker preserved on an ambiguous settle error, removed on a claimed/gone
// rollback) rather than only the Result counters. It derives the name the same
// way writeMarkers does, so created[i] is precisely the path rollback removes.
type fsWriter struct {
	created []string
}

func (w *fsWriter) WriteLRC(song models.Song, filename, outdir string) error {
	name, err := lyrics.SidecarName(song.Track.ArtistName, song.Track.TrackName, filename, false)
	if err != nil {
		return err
	}
	p := filepath.Join(outdir, name)
	if err := os.WriteFile(p, []byte("[au:instrumental]\n"), 0o644); err != nil {
		return err
	}
	w.created = append(w.created, p)
	return nil
}

// fakeStore wraps a real *queue.DBQueue so the common paths (list, reset)
// behave exactly like production, while letting a test force
// ListVocalGateRejections/SettleInstrumental/ResetInstrumentalToUnclassified
// into an outcome or error a live SQLite queue will not produce on demand
// (a peer-claim race, a vanished row, a listing/settle/reset failure).
type fakeStore struct {
	*queue.DBQueue
	listErr        error
	settleOutcome  *queue.SettleOutcome
	settleErr      error
	resetErr       error
	unsettleErr    error
	unsettleResult *bool
}

func (f *fakeStore) ListVocalGateRejections(ctx context.Context, opts queue.ListVocalGateRejectionsOptions) ([]queue.StampedRejection, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.DBQueue.ListVocalGateRejections(ctx, opts)
}

func (f *fakeStore) SettleInstrumental(ctx context.Context, id int64, tel queue.InstrumentalTelemetry) (queue.SettleOutcome, error) {
	if f.settleErr != nil {
		return queue.SettleFailed, f.settleErr
	}
	if f.settleOutcome != nil {
		return *f.settleOutcome, nil
	}
	return f.DBQueue.SettleInstrumental(ctx, id, tel)
}

func (f *fakeStore) ResetInstrumentalToUnclassified(ctx context.Context, id int64) (bool, error) {
	if f.resetErr != nil {
		return false, f.resetErr
	}
	return f.DBQueue.ResetInstrumentalToUnclassified(ctx, id)
}

func (f *fakeStore) ListVocalGateConfirmations(ctx context.Context, opts queue.ListVocalGateConfirmationsOptions) ([]queue.StampedRejection, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.DBQueue.ListVocalGateConfirmations(ctx, opts)
}

// UnsettleInstrumental lets a test force the two outcomes a live SQLite queue
// will not produce on demand: a hard failure, and the worker-claimed race where
// the guarded UPDATE matches nothing between the listing and the mutation.
func (f *fakeStore) UnsettleInstrumental(ctx context.Context, id int64) (bool, error) {
	if f.unsettleErr != nil {
		return false, f.unsettleErr
	}
	if f.unsettleResult != nil {
		return *f.unsettleResult, nil
	}
	return f.DBQueue.UnsettleInstrumental(ctx, id)
}

// seedRejection mirrors the queue-package seed pattern (see
// internal/queue/queue_recalib_test.go's seedDeferredRow +
// TestListVocalGateRejections): enqueue, dequeue, defer, then stamp the
// not-instrumental verdict with the given telemetry. Returns the row id.
func seedRejection(t *testing.T, q *queue.DBQueue, sourcePath string, tel queue.InstrumentalTelemetry) int64 {
	t.Helper()
	ctx := context.Background()
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:     "out",
		Filename:   "a.lrc",
		SourcePath: sourcePath,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, item.ID, time.Hour, errors.New("no results found")); err != nil {
		t.Fatalf("defer: %v", err)
	}
	if _, err := q.StampUnclassifiedMiss(ctx, item.ID, tel); err != nil {
		t.Fatalf("stamp unclassified miss: %v", err)
	}
	return item.ID
}

// openTestQueueWithDB is openTestQueue plus the raw handle, for tests that
// assert on columns the Store seam does not expose.
func openTestQueueWithDB(t *testing.T) (*queue.DBQueue, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := dbpkg.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return queue.NewDBQueue(sqlDB), sqlDB
}

func openTestQueue(t *testing.T) *queue.DBQueue {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := dbpkg.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return queue.NewDBQueue(sqlDB)
}

func TestRun_SettlesPassingVersionMatchedRow(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	id := seedRejection(t, q, "/music/violin.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})

	w := &fakeWriter{}
	r := New(q, w)
	// New threshold 0.30 > stored 0.04 => now passes; version matches => settle.
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Settled != 1 || res.MarkersWritten != 1 || w.calls != 1 {
		t.Fatalf("expected 1 settled + 1 marker, got %+v (writer calls %d)", res, w.calls)
	}

	// The settled row must no longer appear as a vocal-gate rejection: it is
	// 'done' now, not 'deferred'.
	rows, err := q.ListVocalGateRejections(ctx, queue.ListVocalGateRejectionsOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, row := range rows {
		if row.ID == id {
			t.Fatalf("expected settled row %d to no longer be a vocal-gate rejection", id)
		}
	}
}

func TestRun_ResetsPassingVersionMismatchedRow(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	seedRejection(t, q, "/music/cello.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0",
	})

	w := &fakeWriter{}
	r := New(q, w)
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ResetStale != 1 || res.Settled != 0 || res.MarkersWritten != 0 || w.calls != 0 {
		t.Fatalf("expected 1 reset-stale and no marker, got %+v (writer calls %d)", res, w.calls)
	}

	// The next reconcile should see it as never-classified (instrumental_result = NULL).
	rows, err := q.ListVocalGateRejections(ctx, queue.ListVocalGateRejectionsOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected the reset row to no longer appear as a vocal-gate rejection, got %+v", rows)
	}
}

func TestRun_SkipsStillNonPassingRow(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	seedRejection(t, q, "/music/spoken.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.50, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})

	w := &fakeWriter{}
	r := New(q, w)
	// VocalMax 0.30 < stored VocalPeak 0.50 => still fails the vocal gate.
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Settled != 0 || res.ResetStale != 0 || res.MarkersWritten != 0 || w.calls != 0 {
		t.Fatalf("expected a full skip, got %+v (writer calls %d)", res, w.calls)
	}
}

func TestRun_DryRunDoesNotMutate(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	seedRejection(t, q, "/music/harp.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})

	w := &fakeWriter{}
	r := New(q, w)
	previewed := 0
	res, err := r.Run(ctx, Options{
		DryRun: true, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
		Preview: func(c Change) {
			previewed++
			if c.Action != "settle" {
				t.Fatalf("expected settle preview, got %q", c.Action)
			}
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Settled != 0 || res.ResetStale != 0 || res.MarkersWritten != 0 || w.calls != 0 || previewed != 1 {
		t.Fatalf("expected a preview-only pass, got %+v (writer calls %d, previewed %d)", res, w.calls, previewed)
	}

	// Nothing mutated: the row is still listed as a vocal-gate rejection.
	rows, err := q.ListVocalGateRejections(ctx, queue.ListVocalGateRejectionsOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the row to still be a vocal-gate rejection after a dry run, got %+v", rows)
	}
}

// TestRun_SkipsGuardDegradedRow verifies a guard-degraded row (the vocal gate
// could not run cleanly, e.g. a legacy mean-only sidecar with
// maxAvailable=false: stored vocal_class="" and vocal_peak=0) is not settled
// by Run, because ListVocalGateRejections no longer returns it. Re-deciding
// such a row from detector.Instrumental(music, vocalPeak, speechMean, ...)
// alone would ignore the live detector's maxAvailable/baselineComplete guards
// and could wrongly settle it as instrumental even though music/speech both
// pass, since vocal_peak=0 spuriously satisfies the vocal-gate check.
func TestRun_SkipsGuardDegradedRow(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	id := seedRejection(t, q, "/music/degraded.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.0, SpeechMean: 0.001, VocalClass: "", DetectorVersion: "1.18.0",
	})

	w := &fakeWriter{}
	r := New(q, w)
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Settled != 0 || res.MarkersWritten != 0 || w.calls != 0 {
		t.Fatalf("expected the guard-degraded row not to be settled, got %+v (writer calls %d)", res, w.calls)
	}

	// SettleInstrumental (the only path that would flip instrumental_result to 1)
	// also flips status to 'done', so the row remaining 'deferred' confirms it
	// was never settled and instrumental_result is untouched at 0.
	deferred, err := q.List(ctx, queue.ListFilter{Status: "deferred"})
	if err != nil {
		t.Fatalf("list deferred: %v", err)
	}
	found := false
	for _, item := range deferred {
		if item.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected guard-degraded row %d to remain deferred (instrumental_result=0), got deferred rows %+v", id, deferred)
	}
}

func TestRun_ReportErrorSkipsRowButNotFatal(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	seedRejection(t, q, "/music/oboe.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})

	w := &fakeWriter{}
	r := New(q, w)
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
		Report: func(Change) error { return errors.New("backup failed") },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Errors != 1 || res.Settled != 0 || w.calls != 0 {
		t.Fatalf("expected a counted error and no mutation, got %+v (writer calls %d)", res, w.calls)
	}
}

func TestRun_ListErrorReturnsError(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)
	store := &fakeStore{DBQueue: q, listErr: errors.New("list failed")}

	w := &fakeWriter{}
	r := New(store, w)
	_, err := r.Run(ctx, Options{MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0"})
	if err == nil {
		t.Fatal("expected an error from a failing list")
	}
}

// TestRun_WriteMarkerErrorCountsErrorNoSettle verifies a Writer failure is
// counted as a non-fatal per-row error and never reaches SettleInstrumental
// (the row stays deferred, not done).
func TestRun_WriteMarkerErrorCountsErrorNoSettle(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	id := seedRejection(t, q, "/music/bassoon.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})

	w := &fakeWriter{err: errors.New("write failed")}
	r := New(q, w)
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Errors != 1 || res.Settled != 0 || res.MarkersWritten != 0 {
		t.Fatalf("expected a counted error and no settle, got %+v", res)
	}

	deferred, err := q.List(ctx, queue.ListFilter{Status: "deferred"})
	if err != nil {
		t.Fatalf("list deferred: %v", err)
	}
	found := false
	for _, item := range deferred {
		if item.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected row %d to remain deferred (a marker-write failure must not settle the row), got deferred rows %+v", id, deferred)
	}
}

// TestRun_SettleErrorLeavesMarkerAndCountsError covers the AMBIGUOUS branch:
// SettleInstrumental itself errors after the marker was written. The marker
// must be left in place (an orphan is recoverable; a wrongly-deleted valid
// result is not) and the row counted as an error.
func TestRun_SettleErrorLeavesMarkerAndCountsError(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	seedRejection(t, q, "/music/clarinet.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})
	store := &fakeStore{DBQueue: q, settleErr: errors.New("commit failed")}

	w := &fakeWriter{}
	r := New(store, w)
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Errors != 1 || res.Settled != 0 || res.MarkersWritten != 1 || w.calls != 1 {
		t.Fatalf("expected 1 counted error and the marker left in place, got %+v (writer calls %d)", res, w.calls)
	}
}

// TestRun_SettleClaimedRollsBackMarker covers the SettleClaimed arm: a
// serve-mode worker claimed the row mid-recalibration, so nothing was
// written to the DB and this run's orphan marker must be rolled back.
func TestRun_SettleClaimedRollsBackMarker(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	seedRejection(t, q, "/music/trombone.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})
	outcome := queue.SettleClaimed
	store := &fakeStore{DBQueue: q, settleOutcome: &outcome}

	w := &fakeWriter{}
	r := New(store, w)
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.SkippedClaimed != 1 || res.Settled != 0 || res.MarkersWritten != 0 || res.Errors != 0 {
		t.Fatalf("expected the orphan marker rolled back and 1 skipped-claimed, got %+v", res)
	}
}

// TestRun_SettleRowGoneRollsBackMarker covers the SettleRowGone arm: the row
// vanished (e.g. pruned) mid-recalibration, so the marker this run wrote is
// an orphan and must come back off.
func TestRun_SettleRowGoneRollsBackMarker(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	seedRejection(t, q, "/music/tuba.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})
	outcome := queue.SettleRowGone
	store := &fakeStore{DBQueue: q, settleOutcome: &outcome}

	w := &fakeWriter{}
	r := New(store, w)
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.SkippedClaimed != 1 || res.Settled != 0 || res.MarkersWritten != 0 || res.Errors != 0 {
		t.Fatalf("expected the orphan marker rolled back and 1 skipped-claimed, got %+v", res)
	}
}

// TestRun_SettleClaimedRemovesMarkerFile is the on-disk counterpart to
// TestRun_SettleClaimedRollsBackMarker: with a writer that actually creates the
// sidecar, a claimed row's orphan marker must be gone from the filesystem after
// the rollback, not merely uncounted.
func TestRun_SettleClaimedRemovesMarkerFile(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)
	src := filepath.Join(t.TempDir(), "trombone.flac")
	seedRejection(t, q, src, queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})
	outcome := queue.SettleClaimed
	store := &fakeStore{DBQueue: q, settleOutcome: &outcome}

	w := &fsWriter{}
	r := New(store, w)
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(w.created) != 1 {
		t.Fatalf("expected the writer to create exactly 1 marker, got %d", len(w.created))
	}
	if _, statErr := os.Stat(w.created[0]); !os.IsNotExist(statErr) {
		t.Fatalf("claimed rollback must remove the orphan marker %s (stat err = %v)", w.created[0], statErr)
	}
	if res.SkippedClaimed != 1 || res.MarkersWritten != 0 {
		t.Fatalf("expected 1 skipped-claimed and 0 markers retained, got %+v", res)
	}
}

// TestRun_SettleErrorPreservesMarkerFile is the on-disk counterpart to
// TestRun_SettleErrorLeavesMarkerAndCountsError: an ambiguous settle error must
// leave the marker on disk (a recoverable orphan beats a wrongly-deleted valid
// result).
func TestRun_SettleErrorPreservesMarkerFile(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)
	src := filepath.Join(t.TempDir(), "clarinet.flac")
	seedRejection(t, q, src, queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})
	store := &fakeStore{DBQueue: q, settleErr: errors.New("commit failed")}

	w := &fsWriter{}
	r := New(store, w)
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(w.created) != 1 {
		t.Fatalf("expected the writer to create exactly 1 marker, got %d", len(w.created))
	}
	if _, statErr := os.Stat(w.created[0]); statErr != nil {
		t.Fatalf("ambiguous settle error must preserve the marker %s (stat err = %v)", w.created[0], statErr)
	}
	if res.Errors != 1 || res.MarkersWritten != 1 {
		t.Fatalf("expected 1 counted error and the marker kept, got %+v", res)
	}
}

// TestRun_SettleAlreadyInstrumentalKeepsMarker covers the
// SettleAlreadyInstrumental arm: a peer backfill settled the row first with
// the same verdict, so this run's (byte-identical) marker is correct and
// must NOT be removed.
func TestRun_SettleAlreadyInstrumentalKeepsMarker(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	seedRejection(t, q, "/music/piccolo.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})
	outcome := queue.SettleAlreadyInstrumental
	store := &fakeStore{DBQueue: q, settleOutcome: &outcome}

	w := &fakeWriter{}
	r := New(store, w)
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Settled != 0 || res.SkippedClaimed != 0 || res.Errors != 0 || res.MarkersWritten != 1 {
		t.Fatalf("expected the marker kept and no error/skip/settle counted, got %+v", res)
	}
}

// TestRun_ResetInstrumentalToUnclassifiedErrorCountsError covers the
// reset-stale path's error branch: ResetInstrumentalToUnclassified itself
// errors.
func TestRun_ResetInstrumentalToUnclassifiedErrorCountsError(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	seedRejection(t, q, "/music/viola.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0",
	})
	store := &fakeStore{DBQueue: q, resetErr: errors.New("reset failed")}

	w := &fakeWriter{}
	r := New(store, w)
	res, err := r.Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Errors != 1 || res.ResetStale != 0 || w.calls != 0 {
		t.Fatalf("expected a counted reset error, got %+v (writer calls %d)", res, w.calls)
	}
}

// seedConfirmation seeds a row the detector SETTLED instrumental and writes the
// marker sidecar next to its source, with the given provenance header, so a
// reverse run has a real file to act on.
func seedConfirmation(t *testing.T, q *queue.DBQueue, sourcePath string, tel queue.InstrumentalTelemetry, source string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:     "out",
		Filename:   "a.lrc",
		SourcePath: sourcePath,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, item.ID, time.Hour, errors.New("no results found")); err != nil {
		t.Fatalf("defer: %v", err)
	}
	if _, err := q.SettleInstrumental(ctx, item.ID, tel); err != nil {
		t.Fatalf("settle: %v", err)
	}
	name, err := lyrics.SidecarName("Artist", "Title", filepath.Base(sourcePath), false)
	if err != nil {
		t.Fatalf("sidecar name: %v", err)
	}
	marker := filepath.Join(filepath.Dir(sourcePath), name)
	body := "[by:canticle]\n[source:" + source + "]\n" + lyrics.InstrumentalMarker + "\n"
	if err := os.WriteFile(marker, []byte(body), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return item.ID, marker
}

// TestReverse_RevertsRowThatNowFailsTheVocalGate is the tightening direction:
// a confirmed instrumental whose stored vocal_peak now sits at or above a
// LOWERED VocalMax goes back to deferred and its detector marker comes off disk.
func TestReverse_RevertsRowThatNowFailsTheVocalGate(t *testing.T) {
	ctx := context.Background()
	q, sqlDB := openTestQueueWithDB(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.flac")
	tel := queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.02, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "v1"}
	id, marker := seedConfirmation(t, q, src, tel, lyrics.SourceDetector)

	rc := New(q, &fakeWriter{})
	res, err := rc.Reverse(ctx, Options{
		MinConfidence: 0.9, VocalMax: 0.015, SpeechMax: 0.2, CurrentVersion: "v1",
	})
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if res.Reversed != 1 {
		t.Errorf("Reversed = %d; want 1", res.Reversed)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("marker still on disk; want removed once the verdict is reversed")
	}
	var status string
	var result int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, instrumental_result FROM work_queue WHERE id = ?`, id,
	).Scan(&status, &result); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "deferred" || result != 0 {
		t.Errorf("status/result = %q/%d; want deferred/0", status, result)
	}
}

// TestReverse_LeavesRowThatStillPassesTheGate pins that a still-correct
// instrumental is not disturbed.
func TestReverse_LeavesRowThatStillPassesTheGate(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.flac")
	tel := queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.001, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "v1"}
	_, marker := seedConfirmation(t, q, src, tel, lyrics.SourceDetector)

	rc := New(q, &fakeWriter{})
	res, err := rc.Reverse(ctx, Options{
		MinConfidence: 0.9, VocalMax: 0.015, SpeechMax: 0.2, CurrentVersion: "v1",
	})
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if res.Reversed != 0 {
		t.Errorf("Reversed = %d; want 0 -- the row still passes the tightened gate", res.Reversed)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("marker removed; want preserved for a still-passing row")
	}
}

// TestReverse_PreservesProviderDeclaredInstrumental is the safety property: a
// marker the PROVIDER wrote is editorially authoritative, not the detector's to
// re-decide. It must survive a reverse run untouched, and its row must not be
// reopened, even when the stored telemetry would fail the tightened gate.
func TestReverse_PreservesProviderDeclaredInstrumental(t *testing.T) {
	ctx := context.Background()
	q, sqlDB := openTestQueueWithDB(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.flac")
	tel := queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.02, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "v1"}
	id, marker := seedConfirmation(t, q, src, tel, "musixmatch")

	rc := New(q, &fakeWriter{})
	res, err := rc.Reverse(ctx, Options{
		MinConfidence: 0.9, VocalMax: 0.015, SpeechMax: 0.2, CurrentVersion: "v1",
	})
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if res.Reversed != 0 {
		t.Errorf("Reversed = %d; want 0 -- a provider-declared instrumental is not the detector's to reverse", res.Reversed)
	}
	if res.SkippedProviderOwned != 1 {
		t.Errorf("SkippedProviderOwned = %d; want 1", res.SkippedProviderOwned)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("provider marker removed; want preserved")
	}
	var status string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status FROM work_queue WHERE id = ?`, id,
	).Scan(&status); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "done" {
		t.Errorf("status = %q; want done -- the provider verdict stands", status)
	}
}

// TestReverse_ReversesLegacyBareMarker pins that a marker predating provenance
// stamping is still the detector's to remove. The row carries
// instrumental_result=1, which only SettleInstrumental sets, so the DATABASE
// already proves the detector owns the verdict; an absent [source:] header is
// an artifact of when the file was written, not evidence of provider ownership.
// Treating bare as provider-owned stranded 95% of a real recalibration backlog.
func TestReverse_ReversesLegacyBareMarker(t *testing.T) {
	ctx := context.Background()
	q, _ := openTestQueueWithDB(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.flac")
	tel := queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.02, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "v1"}

	// Seed with a BARE marker: the marker line only, no provenance header.
	id, err := func() (int64, error) {
		item, err := q.Enqueue(ctx, models.Inputs{
			Track:      models.Track{ArtistName: "Artist", TrackName: "Title"},
			Outdir:     "out",
			Filename:   "a.lrc",
			SourcePath: src,
		}, queue.PriorityScan)
		if err != nil {
			return 0, err
		}
		if _, err := q.Dequeue(ctx); err != nil {
			return 0, err
		}
		if _, err := q.Defer(ctx, item.ID, time.Hour, errors.New("no results found")); err != nil {
			return 0, err
		}
		if _, err := q.SettleInstrumental(ctx, item.ID, tel); err != nil {
			return 0, err
		}
		return item.ID, nil
	}()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	name, err := lyrics.SidecarName("Artist", "Title", "a.flac", false)
	if err != nil {
		t.Fatalf("sidecar name: %v", err)
	}
	marker := filepath.Join(dir, name)
	if err := os.WriteFile(marker, []byte(lyrics.InstrumentalMarker+"\n"), 0o644); err != nil {
		t.Fatalf("write bare marker: %v", err)
	}

	rc := New(q, &fakeWriter{})
	res, err := rc.Reverse(ctx, Options{
		MinConfidence: 0.9, VocalMax: 0.015, SpeechMax: 0.2, CurrentVersion: "v1",
	})
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if res.Reversed != 1 {
		t.Errorf("Reversed = %d; want 1 -- a bare marker on an instrumental_result=1 row is the detector's", res.Reversed)
	}
	if res.SkippedProviderOwned != 0 {
		t.Errorf("SkippedProviderOwned = %d; want 0", res.SkippedProviderOwned)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("bare marker still on disk; want removed")
	}
	_ = id
}

// TestReverse_MissingMarkerStillRevertsRow pins that an absent sidecar does not
// block the database correction. There is nothing to delete, but the row is
// still wrongly settled, and leaving it settled would strand it forever.
func TestReverse_MissingMarkerStillRevertsRow(t *testing.T) {
	ctx := context.Background()
	q, sqlDB := openTestQueueWithDB(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.flac")
	tel := queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.02, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "v1"}
	id, marker := seedConfirmation(t, q, src, tel, lyrics.SourceDetector)
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}

	res, err := New(q, &fakeWriter{}).Reverse(ctx, Options{
		MinConfidence: 0.9, VocalMax: 0.015, SpeechMax: 0.2, CurrentVersion: "v1",
	})
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if res.Reversed != 1 {
		t.Errorf("Reversed = %d; want 1 even with no marker on disk", res.Reversed)
	}
	if res.MarkersRemoved != 0 {
		t.Errorf("MarkersRemoved = %d; want 0 -- nothing was there to remove", res.MarkersRemoved)
	}
	if res.Errors != 0 {
		t.Errorf("Errors = %d; want 0 -- an absent marker is not an error", res.Errors)
	}
	var status string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "deferred" {
		t.Errorf("status = %q; want deferred", status)
	}
}

// TestReverse_LeavesForeignSidecarAlone pins that a sidecar which is not an
// instrumental marker at all is never deleted. Something else owns that file,
// and the reverse pass must not touch it or the row behind it.
func TestReverse_LeavesForeignSidecarAlone(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.flac")
	tel := queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.02, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "v1"}
	_, marker := seedConfirmation(t, q, src, tel, lyrics.SourceDetector)
	// Overwrite the marker with ordinary sidecar content: no marker line.
	if err := os.WriteFile(marker, []byte("just some text, not a marker\n"), 0o644); err != nil {
		t.Fatalf("write foreign sidecar: %v", err)
	}

	res, err := New(q, &fakeWriter{}).Reverse(ctx, Options{
		MinConfidence: 0.9, VocalMax: 0.015, SpeechMax: 0.2, CurrentVersion: "v1",
	})
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if res.Reversed != 0 || res.SkippedProviderOwned != 1 {
		t.Errorf("Reversed=%d SkippedProviderOwned=%d; want 0/1 -- a non-marker sidecar is not ours",
			res.Reversed, res.SkippedProviderOwned)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("foreign sidecar removed; want preserved")
	}
}

// TestReverse_DryRunPreviewsWithoutMutating pins that a dry run reports through
// Preview and changes neither the database nor the disk.
func TestReverse_DryRunPreviewsWithoutMutating(t *testing.T) {
	ctx := context.Background()
	q, sqlDB := openTestQueueWithDB(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.flac")
	tel := queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.02, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "v1"}
	id, marker := seedConfirmation(t, q, src, tel, lyrics.SourceDetector)

	previewed := 0
	res, err := New(q, &fakeWriter{}).Reverse(ctx, Options{
		DryRun: true, MinConfidence: 0.9, VocalMax: 0.015, SpeechMax: 0.2, CurrentVersion: "v1",
		Preview: func(c Change) {
			previewed++
			if c.Action != "reverse" {
				t.Errorf("Action = %q; want reverse", c.Action)
			}
		},
		Report: func(Change) error {
			t.Fatal("Report must not run in a dry run -- it is the durable backup record")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if previewed != 1 || res.Reversed != 0 {
		t.Errorf("previewed=%d Reversed=%d; want 1/0 (counters are apply-only)", previewed, res.Reversed)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("marker removed during a dry run")
	}
	var status string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "done" {
		t.Errorf("status = %q; want done -- a dry run must not mutate", status)
	}
}

// TestReverse_ReportFailureLeavesRowAndMarkerIntact pins the backup-first rule
// in the reverse direction: if the restorable record cannot be written, the
// change must not happen at all.
func TestReverse_ReportFailureLeavesRowAndMarkerIntact(t *testing.T) {
	ctx := context.Background()
	q, sqlDB := openTestQueueWithDB(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.flac")
	tel := queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.02, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "v1"}
	id, marker := seedConfirmation(t, q, src, tel, lyrics.SourceDetector)

	res, err := New(q, &fakeWriter{}).Reverse(ctx, Options{
		MinConfidence: 0.9, VocalMax: 0.015, SpeechMax: 0.2, CurrentVersion: "v1",
		Report: func(Change) error { return errors.New("backup disk full") },
	})
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if res.Reversed != 0 || res.Errors != 1 {
		t.Errorf("Reversed=%d Errors=%d; want 0/1", res.Reversed, res.Errors)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("marker removed despite the backup failing")
	}
	var status string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "done" {
		t.Errorf("status = %q; want done -- no backup, no change", status)
	}
}

// TestReverse_SkipsRowClaimedMidRun pins the race window: when the guarded
// UPDATE matches nothing because a worker re-claimed the row between the
// listing and the mutation, the marker must be left alone rather than deleted
// out from under whoever now owns the row.
func TestReverse_SkipsRowClaimedMidRun(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.flac")
	tel := queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.02, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "v1"}
	_, marker := seedConfirmation(t, q, src, tel, lyrics.SourceDetector)

	claimed := false
	store := &fakeStore{DBQueue: q, unsettleResult: &claimed}
	res, err := New(store, &fakeWriter{}).Reverse(ctx, Options{
		MinConfidence: 0.9, VocalMax: 0.015, SpeechMax: 0.2, CurrentVersion: "v1",
	})
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if res.Reversed != 0 || res.SkippedClaimed != 1 {
		t.Errorf("Reversed=%d SkippedClaimed=%d; want 0/1", res.Reversed, res.SkippedClaimed)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("marker removed for a row this run did not actually revert")
	}
}

// TestReverse_UnsettleErrorIsCountedNotFatal pins that a per-row store failure
// is counted and the run continues, matching Run's per-row error contract.
func TestReverse_UnsettleErrorIsCountedNotFatal(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.flac")
	tel := queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.02, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "v1"}
	_, marker := seedConfirmation(t, q, src, tel, lyrics.SourceDetector)

	store := &fakeStore{DBQueue: q, unsettleErr: errors.New("database is locked")}
	res, err := New(store, &fakeWriter{}).Reverse(ctx, Options{
		MinConfidence: 0.9, VocalMax: 0.015, SpeechMax: 0.2, CurrentVersion: "v1",
	})
	if err != nil {
		t.Fatalf("Reverse: a per-row failure must not abort the run: %v", err)
	}
	if res.Errors != 1 || res.Reversed != 0 {
		t.Errorf("Errors=%d Reversed=%d; want 1/0", res.Errors, res.Reversed)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("marker removed after the database write failed")
	}
}

// TestReverse_ListErrorAborts pins the one failure that IS fatal: if the
// candidate set cannot be enumerated, the run has nothing trustworthy to act on.
func TestReverse_ListErrorAborts(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)
	store := &fakeStore{DBQueue: q, listErr: errors.New("no such table")}
	if _, err := New(store, &fakeWriter{}).Reverse(ctx, Options{
		MinConfidence: 0.9, VocalMax: 0.015, SpeechMax: 0.2, CurrentVersion: "v1",
	}); err == nil {
		t.Fatal("Reverse: want an error when the listing fails")
	}
}
