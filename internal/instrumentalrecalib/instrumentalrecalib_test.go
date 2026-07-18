package instrumentalrecalib

import (
	"context"
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
	listErr       error
	settleOutcome *queue.SettleOutcome
	settleErr     error
	resetErr      error
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
