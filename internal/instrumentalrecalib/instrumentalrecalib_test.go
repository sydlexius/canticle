package instrumentalrecalib

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	dbpkg "github.com/doxazo-net/canticle/internal/db"
	"github.com/doxazo-net/canticle/internal/models"
	"github.com/doxazo-net/canticle/internal/queue"
)

type fakeWriter struct{ calls int }

func (f *fakeWriter) WriteLRC(_ models.Song, _ string, _ string) error { f.calls++; return nil }

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
