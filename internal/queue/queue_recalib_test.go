package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/doxazo-net/canticle/internal/models"
)

// seedDeferredRow enqueues a work_queue row, claims it (Dequeue), then defers
// it (the standard path other queue tests use to reach StatusDeferred; see
// TestDBQueue_NoResultRequeueIsDeferredButReprocessable), returning its id.
func seedDeferredRow(t *testing.T, q *DBQueue, artist, title, sourcePath string) int64 {
	t.Helper()
	ctx := context.Background()
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: artist, TrackName: title},
		Outdir:     "out",
		Filename:   "a.lrc",
		SourcePath: sourcePath,
	}, PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, item.ID, time.Hour, errors.New("no results found")); err != nil {
		t.Fatalf("defer: %v", err)
	}
	return item.ID
}

func TestListVocalGateRejections(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	// Seed one deferred row, stamp it not-instrumental with telemetry.
	id := seedDeferredRow(t, q, "Artist", "Title", "/music/a.flac")
	if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0"}); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	got, err := q.ListVocalGateRejections(ctx, ListVocalGateRejectionsOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Tel.VocalPeak != 0.04 || got[0].ID != id {
		t.Fatalf("unexpected rows: %+v", got)
	}
}

// TestListVocalGateRejections_ExcludesEmptyVocalClass verifies a row whose
// vocal gate could not run cleanly (e.g. a legacy mean-only sidecar with
// maxAvailable=false, stamped with an empty vocal_class) is excluded, since
// that guard-completeness state cannot be reconstructed from stored scores
// alone and re-deciding it risks a false instrumental marker.
func TestListVocalGateRejections_ExcludesEmptyVocalClass(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	id := seedDeferredRow(t, q, "Degraded Artist", "Degraded", "/music/degraded.flac")
	if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.0, SpeechMean: 0.001, VocalClass: "", DetectorVersion: "1.17.0"}); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	got, err := q.ListVocalGateRejections(ctx, ListVocalGateRejectionsOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty-vocal_class row to be excluded, got: %+v", got)
	}
}

// TestListVocalGateRejections_ExcludesNullTelemetry verifies a legacy row
// stamped not-instrumental before the telemetry columns were populated
// (instrumental_result=0 but music_sum/vocal_peak/speech_mean still NULL) is
// excluded: it cannot be re-decided from stored scores alone. Raw SQL is used
// here (rather than StampUnclassifiedMiss, which always writes concrete
// telemetry values) to reproduce that legacy NULL-telemetry shape.
func TestListVocalGateRejections_ExcludesNullTelemetry(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	id := seedDeferredRow(t, q, "Artist", "NeverScored", "/music/never-scored.flac")
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET instrumental_result = 0 WHERE id = ?`, id); err != nil {
		t.Fatalf("stamp legacy null telemetry: %v", err)
	}
	got, err := q.ListVocalGateRejections(ctx, ListVocalGateRejectionsOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected NULL-telemetry row to be excluded, got: %+v", got)
	}
}

func TestResetInstrumentalToUnclassified(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	id := seedDeferredRow(t, q, "Artist", "Title", "/music/a.flac")
	if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0"}); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if reset, err := q.ResetInstrumentalToUnclassified(ctx, id); err != nil || !reset {
		t.Fatalf("reset: got %v, %v", reset, err)
	}
}
