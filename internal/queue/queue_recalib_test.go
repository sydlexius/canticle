package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
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

// TestStampInstrumentalTelemetry covers the #557 write: a row that already
// carries an instrumental verdict but no evidence gains its scores without the
// verdict changing. These rows were decided before migration 025 added the
// telemetry columns, and every re-decision path is arithmetic over those scores,
// so until this write exists they are skipped by construction forever.
func TestStampInstrumentalTelemetry(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	id := seedDeferredRow(t, q, "Artist", "Title", "/music/a.flac")

	// Reproduce the pre-025 shape: verdict persisted, telemetry NULL.
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET instrumental_result = 1, status = 'done' WHERE id = ?`, id); err != nil {
		t.Fatalf("seed pre-telemetry row: %v", err)
	}
	var music, vocal, speech *float64
	if err := q.db.QueryRowContext(ctx,
		`SELECT music_sum, vocal_peak, speech_mean FROM work_queue WHERE id = ?`, id,
	).Scan(&music, &vocal, &speech); err != nil {
		t.Fatalf("read initial telemetry: %v", err)
	}
	if music != nil || vocal != nil || speech != nil {
		t.Fatalf("precondition: telemetry should start NULL, got (%v,%v,%v)", music, vocal, speech)
	}

	// A SECOND unscored instrumental row, never passed to the stamp. It must come
	// back untouched: without it, a WHERE clause that matched every row (or the
	// wrong row) would satisfy every other assertion here.
	otherID := seedDeferredRow(t, q, "Other", "Other Title", "/music/b.flac")
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET instrumental_result = 1, status = 'done' WHERE id = ?`, otherID); err != nil {
		t.Fatalf("seed second row: %v", err)
	}

	tel := InstrumentalTelemetry{MusicSum: 0.91, VocalPeak: 0.02, SpeechMean: 0.003, VocalClass: "Singing", DetectorVersion: "1.26.0"}
	stamped, err := q.StampInstrumentalTelemetry(ctx, id, tel)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if !stamped {
		t.Fatal("stamped = false; want true for a row still carrying the verdict")
	}

	var result int
	var status, vocalClass, dv string
	if err := q.db.QueryRowContext(ctx,
		`SELECT instrumental_result, status, music_sum, vocal_peak, speech_mean, vocal_class, detector_version
         FROM work_queue WHERE id = ?`, id,
	).Scan(&result, &status, &music, &vocal, &speech, &vocalClass, &dv); err != nil {
		t.Fatalf("read stamped telemetry: %v", err)
	}
	// The verdict must be untouched: this write attaches evidence, it never
	// re-decides. A stamp that flipped the verdict would silently rewrite history.
	if result != 1 {
		t.Errorf("instrumental_result = %d; want 1 (unchanged)", result)
	}
	// Nor may it move the row's status: re-queuing a completed instrumental as a
	// side effect of a backfill would be invisible in the summary.
	if status != "done" {
		t.Errorf("status = %q; want \"done\" (unchanged)", status)
	}
	if music == nil || *music != tel.MusicSum {
		t.Errorf("music_sum = %v; want %v", music, tel.MusicSum)
	}
	if vocal == nil || *vocal != tel.VocalPeak {
		t.Errorf("vocal_peak = %v; want %v", vocal, tel.VocalPeak)
	}
	// Asserted exactly, not merely non-nil: a distinct value per field is what
	// catches one telemetry field being wired from another's source.
	if speech == nil || *speech != tel.SpeechMean {
		t.Errorf("speech_mean = %v; want %v", speech, tel.SpeechMean)
	}
	if vocalClass != tel.VocalClass || dv != tel.DetectorVersion {
		t.Errorf("vocal_class/detector_version = (%q,%q); want (%q,%q)", vocalClass, dv, tel.VocalClass, tel.DetectorVersion)
	}

	var otherMusic *float64
	if err := q.db.QueryRowContext(ctx, `SELECT music_sum FROM work_queue WHERE id = ?`, otherID).Scan(&otherMusic); err != nil {
		t.Fatalf("read second row: %v", err)
	}
	if otherMusic != nil {
		t.Errorf("second row's music_sum = %v; want NULL -- the stamp must touch only the id it was given", *otherMusic)
	}
}

// TestNeedsInstrumentalTelemetryMirrorsTheStamp is the anti-drift test. The dry
// run's honesty depends on this predicate selecting exactly the rows the stamp
// writes, and the two are separate SQL strings that can drift apart silently --
// so each case asserts BOTH answers agree, rather than checking the predicate
// alone. A divergence here means the operator is previewing one set and applying
// another, which is the failure the dry-run contract exists to prevent.
func TestNeedsInstrumentalTelemetryMirrorsTheStamp(t *testing.T) {
	ctx := context.Background()
	tel := InstrumentalTelemetry{MusicSum: 0.91, VocalPeak: 0.02, SpeechMean: 0.003, DetectorVersion: "1.26.0"}

	// Bound as parameters rather than interpolated into the statement: the values
	// are literals here, but the fmt.Sprintf form matches the SAST injection
	// signature and would recur as gate noise on every run.
	cases := []struct {
		name               string
		instrumentalResult sql.NullInt64
		status             string
		musicSum           sql.NullFloat64
		want               bool
	}{
		{"unscored done instrumental", sql.NullInt64{Int64: 1, Valid: true}, "done", sql.NullFloat64{}, true},
		{"already scored", sql.NullInt64{Int64: 1, Valid: true}, "done", sql.NullFloat64{Float64: 0.5, Valid: true}, false},
		{"still processing", sql.NullInt64{Int64: 1, Valid: true}, "processing", sql.NullFloat64{}, false},
		{"not instrumental", sql.NullInt64{Int64: 0, Valid: true}, "done", sql.NullFloat64{}, false},
		{"no verdict", sql.NullInt64{}, "done", sql.NullFloat64{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := NewDBQueue(openQueueTestDB(t))
			id := seedDeferredRow(t, q, "Artist", "Title", "/music/a.flac")
			if _, err := q.db.ExecContext(ctx,
				`UPDATE work_queue SET instrumental_result = ?, status = ?, music_sum = ? WHERE id = ?`,
				tc.instrumentalResult, tc.status, tc.musicSum, id); err != nil {
				t.Fatalf("seed: %v", err)
			}

			needs, err := q.NeedsInstrumentalTelemetry(ctx, id)
			if err != nil {
				t.Fatalf("needs: %v", err)
			}
			if needs != tc.want {
				t.Errorf("NeedsInstrumentalTelemetry = %v; want %v", needs, tc.want)
			}
			// The stamp must reach the same verdict, or the dry run previews a
			// different set than the apply writes.
			stamped, err := q.StampInstrumentalTelemetry(ctx, id, tel)
			if err != nil {
				t.Fatalf("stamp: %v", err)
			}
			if stamped != needs {
				t.Errorf("predicate and stamp disagree: needs=%v stamped=%v -- the dry run would mispreview this row", needs, stamped)
			}
		})
	}

	// A missing row is previewable as "no", never an error.
	q := NewDBQueue(openQueueTestDB(t))
	needs, err := q.NeedsInstrumentalTelemetry(ctx, 999999)
	if err != nil || needs {
		t.Errorf("missing id: got (needs=%v, err=%v); want (false, nil)", needs, err)
	}
}

// TestStampInstrumentalTelemetrySkipsScoredAndProcessing pins the two guard terms
// that are not about the verdict itself.
//
// status='done' mirrors ListInstrumental's own selector, which excludes
// 'processing' because the worker stamps instrumental_result=1 just BEFORE
// Complete -- so a row can carry the verdict while a worker is mid-write, and a
// verdict-only guard would clobber that worker's fresh scores with this run's.
//
// music_sum IS NULL confines the write to the population this exists for: a row
// that already has evidence is not #557's business.
func TestStampInstrumentalTelemetrySkipsScoredAndProcessing(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	tel := InstrumentalTelemetry{MusicSum: 0.91, VocalPeak: 0.02, SpeechMean: 0.003, DetectorVersion: "1.26.0"}

	t.Run("already scored", func(t *testing.T) {
		id := seedDeferredRow(t, q, "Artist", "Scored", "/music/scored.flac")
		if _, err := q.db.ExecContext(ctx,
			`UPDATE work_queue SET instrumental_result = 1, status = 'done', music_sum = 0.5 WHERE id = ?`, id); err != nil {
			t.Fatalf("seed: %v", err)
		}
		stamped, err := q.StampInstrumentalTelemetry(ctx, id, tel)
		if err != nil {
			t.Fatalf("stamp: %v", err)
		}
		if stamped {
			t.Error("stamped = true on a row that already carries telemetry; the backfill must not overwrite it")
		}
		var music float64
		if err := q.db.QueryRowContext(ctx, `SELECT music_sum FROM work_queue WHERE id = ?`, id).Scan(&music); err != nil {
			t.Fatalf("read: %v", err)
		}
		if music != 0.5 {
			t.Errorf("music_sum = %v; want the original 0.5 preserved", music)
		}
	})

	t.Run("still processing", func(t *testing.T) {
		id := seedDeferredRow(t, q, "Artist", "InFlight", "/music/inflight.flac")
		// The worker's shape mid-write: verdict stamped, row not yet completed.
		if _, err := q.db.ExecContext(ctx,
			`UPDATE work_queue SET instrumental_result = 1, status = 'processing' WHERE id = ?`, id); err != nil {
			t.Fatalf("seed: %v", err)
		}
		stamped, err := q.StampInstrumentalTelemetry(ctx, id, tel)
		if err != nil {
			t.Fatalf("stamp: %v", err)
		}
		if stamped {
			t.Error("stamped = true on a 'processing' row; that is the mid-write window ListInstrumental excludes")
		}
		var music *float64
		if err := q.db.QueryRowContext(ctx, `SELECT music_sum FROM work_queue WHERE id = ?`, id).Scan(&music); err != nil {
			t.Fatalf("read: %v", err)
		}
		if music != nil {
			t.Errorf("music_sum = %v; want NULL -- a worker's in-flight row must not be written", *music)
		}
	})
}

// TestStampInstrumentalTelemetryGuardsOnVerdict pins the guard. If the verdict
// changed or was cleared underneath the run -- a concurrent reconcile or a
// worker re-decision -- there is nothing to attach evidence to, and stamping
// anyway would describe a verdict that no longer exists.
func TestStampInstrumentalTelemetryGuardsOnVerdict(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	id := seedDeferredRow(t, q, "Artist", "Title", "/music/a.flac")

	// instrumental_result = 0: the detector looked and said NOT instrumental.
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET instrumental_result = 0 WHERE id = ?`, id); err != nil {
		t.Fatalf("seed non-instrumental row: %v", err)
	}
	stamped, err := q.StampInstrumentalTelemetry(ctx, id,
		InstrumentalTelemetry{MusicSum: 0.91, VocalPeak: 0.02, SpeechMean: 0.003, DetectorVersion: "1.26.0"})
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if stamped {
		t.Error("stamped = true on a row whose verdict is not instrumental; the guard must reject it")
	}
	var music *float64
	if err := q.db.QueryRowContext(ctx, `SELECT music_sum FROM work_queue WHERE id = ?`, id).Scan(&music); err != nil {
		t.Fatalf("read telemetry: %v", err)
	}
	if music != nil {
		t.Errorf("music_sum = %v; want NULL -- nothing should have been written", *music)
	}

	// A missing id is a benign no-op, matching the sibling stamps.
	if stamped, err := q.StampInstrumentalTelemetry(ctx, 999999,
		InstrumentalTelemetry{MusicSum: 0.5, DetectorVersion: "1.26.0"}); err != nil || stamped {
		t.Errorf("missing id: got (stamped=%v, err=%v); want (false, nil)", stamped, err)
	}
}

func TestListVocalGateRejections(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	// Seed one deferred row, stamp it not-instrumental with telemetry.
	id := seedDeferredRow(t, q, "Artist", "Title", "/music/a.flac")
	if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0", GateComplete: true}); err != nil {
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
	if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0", GateComplete: true}); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if reset, err := q.ResetInstrumentalToUnclassified(ctx, id); err != nil || !reset {
		t.Fatalf("reset: got %v, %v", reset, err)
	}

	// The reset must actually clear instrumental_result to NULL, not merely
	// report success, and must leave status/telemetry untouched.
	var status string
	var result *int
	if err := q.db.QueryRowContext(ctx,
		`SELECT status, instrumental_result FROM work_queue WHERE id = ?`, id,
	).Scan(&status, &result); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "deferred" {
		t.Errorf("status = %q; want deferred (unchanged)", status)
	}
	if result != nil {
		t.Errorf("instrumental_result = %v; want NULL after reset", *result)
	}
}

// TestResetInstrumentalToUnclassified_NonDeferredRowNotReset verifies the
// status='deferred' guard: a row a worker has since claimed (status =
// 'processing', not deferred) must not be reset out from under it.
func TestResetInstrumentalToUnclassified_NonDeferredRowNotReset(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	id := seedDeferredRow(t, q, "Artist", "Title", "/music/a.flac")
	if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0", GateComplete: true}); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	// Simulate a worker claiming the row mid-recalibration.
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id); err != nil {
		t.Fatalf("mark processing: %v", err)
	}

	reset, err := q.ResetInstrumentalToUnclassified(ctx, id)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reset {
		t.Fatalf("expected a non-deferred row not to be reset")
	}

	var result *int
	if err := q.db.QueryRowContext(ctx, `SELECT instrumental_result FROM work_queue WHERE id = ?`, id).Scan(&result); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if result == nil || *result != 0 {
		t.Errorf("instrumental_result = %v; want unchanged 0", result)
	}
}

// TestResetInstrumentalToUnclassified_NonZeroVerdictNotReset verifies the
// instrumental_result = 0 guard: a row concurrently re-stamped to a positive
// verdict (result = 1) while still deferred must not have that verdict cleared
// out from under a peer settle.
func TestResetInstrumentalToUnclassified_NonZeroVerdictNotReset(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	id := seedDeferredRow(t, q, "Artist", "Title", "/music/a.flac")
	if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0", GateComplete: true}); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	// Simulate a concurrent positive settle (still deferred) landing between the
	// engine's selection and this reset.
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET instrumental_result = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("set positive: %v", err)
	}

	reset, err := q.ResetInstrumentalToUnclassified(ctx, id)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reset {
		t.Fatalf("expected a row with a positive verdict not to be reset")
	}
	var result *int
	if err := q.db.QueryRowContext(ctx, `SELECT instrumental_result FROM work_queue WHERE id = ?`, id).Scan(&result); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if result == nil || *result != 1 {
		t.Fatalf("instrumental_result = %v; want 1 (preserved)", result)
	}
}

// TestListVocalGateRejections_ScopesToLibrary verifies the --library
// narrowing actually filters the candidate set, mirroring
// TestDBQueue_ListUnclassifiedScopesToLibrary.
func TestListVocalGateRejections_ScopesToLibrary(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	libA, srA := insertLibraryAndScanResult(t, sqlDB, "/libA", "/libA/a.mp3")
	_, srB := insertLibraryAndScanResult(t, sqlDB, "/libB", "/libB/b.mp3")

	mk := func(title, src string, scanResultID int64) int64 {
		item, err := q.Enqueue(ctx, models.Inputs{
			Track:        models.Track{ArtistName: "Artist", TrackName: title},
			Outdir:       "out",
			Filename:     title + ".lrc",
			SourcePath:   src,
			ScanResultID: scanResultID,
		}, PriorityScan)
		if err != nil {
			t.Fatalf("enqueue %s: %v", title, err)
		}
		if _, err := q.Dequeue(ctx); err != nil {
			t.Fatalf("dequeue %s: %v", title, err)
		}
		if _, err := q.Defer(ctx, item.ID, time.Hour, errors.New("no results found")); err != nil {
			t.Fatalf("defer %s: %v", title, err)
		}
		if _, err := q.StampUnclassifiedMiss(ctx, item.ID, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0", GateComplete: true}); err != nil {
			t.Fatalf("stamp %s: %v", title, err)
		}
		return item.ID
	}
	inA := mk("in-lib-a", "/libA/a.mp3", srA)
	_ = mk("in-lib-b", "/libB/b.mp3", srB)

	got, err := q.ListVocalGateRejections(ctx, ListVocalGateRejectionsOptions{LibraryID: &libA})
	if err != nil {
		t.Fatalf("ListVocalGateRejections: %v", err)
	}
	if len(got) != 1 || got[0].ID != inA {
		var ids []int64
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		t.Fatalf("scoped list = %v; want only the libA row %d", ids, inA)
	}
}

// TestListVocalGateRejections_Limit verifies the Limit option actually caps
// the result set.
func TestListVocalGateRejections_Limit(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	for i := 0; i < 3; i++ {
		id := seedDeferredRow(t, q, "Artist", fmt.Sprintf("Title%d", i), fmt.Sprintf("/music/%d.flac", i))
		if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0", GateComplete: true}); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}

	got, err := q.ListVocalGateRejections(ctx, ListVocalGateRejectionsOptions{Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d; want 2 (Limit must cap the result set)", len(got))
	}
}

// TestListVocalGateRejections_AfterID verifies the resume cursor: AfterID
// excludes every row at or below that work_queue id, so a repeated --limit run
// advances past low-id rows a prior pass already examined instead of re-selecting
// them (#516).
func TestListVocalGateRejections_AfterID(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	var ids []int64
	for i := 0; i < 3; i++ {
		id := seedDeferredRow(t, q, "Artist", fmt.Sprintf("Title%d", i), fmt.Sprintf("/music/%d.flac", i))
		if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0", GateComplete: true}); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		ids = append(ids, id)
	}

	// Cursor at the first row's id: only the two higher-id rows remain, in id order.
	got, err := q.ListVocalGateRejections(ctx, ListVocalGateRejectionsOptions{AfterID: ids[0]})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d; want 2 (AfterID must exclude rows <= the cursor)", len(got))
	}
	if got[0].ID != ids[1] || got[1].ID != ids[2] {
		t.Fatalf("got ids [%d %d]; want [%d %d] (ascending, past the cursor)", got[0].ID, got[1].ID, ids[1], ids[2])
	}

	// AfterID composes with Limit: cursor past the first, cap at one -> just ids[1].
	got, err = q.ListVocalGateRejections(ctx, ListVocalGateRejectionsOptions{AfterID: ids[0], Limit: 1})
	if err != nil {
		t.Fatalf("list with limit: %v", err)
	}
	if len(got) != 1 || got[0].ID != ids[1] {
		t.Fatalf("got %+v; want single row id=%d", got, ids[1])
	}
}

func TestListDetectorInstrumentalMarkers(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()

	// Detector-written instrumental: instrumental_result=1, status=done, dv set.
	detID := seedDeferredRow(t, q, "Det Artist", "Det Title", "/music/det.flac")
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET instrumental_result=1, status='done', outcome_type='instrumental', detector_version='1.5.0' WHERE id=?`, detID); err != nil {
		t.Fatalf("seed detector row: %v", err)
	}
	// Provider-written instrumental: outcome_type='instrumental', instrumental_result NULL. Must be EXCLUDED.
	provID := seedDeferredRow(t, q, "Prov Artist", "Prov Title", "/music/prov.flac")
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET outcome_type='instrumental', status='done' WHERE id=?`, provID); err != nil {
		t.Fatalf("seed provider row: %v", err)
	}
	// Never-detected row: instrumental_result NULL, still deferred. Must be EXCLUDED.
	_ = seedDeferredRow(t, q, "Never", "Scored", "/music/never.flac")

	got, err := q.ListDetectorInstrumentalMarkers(ctx, ListInstrumentalMarkersOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly the detector row, got %d: %+v", len(got), got)
	}
	r := got[0]
	if r.ID != detID {
		t.Errorf("ID = %d, want %d", r.ID, detID)
	}
	if r.DetectorVersion != "1.5.0" {
		t.Errorf("DetectorVersion = %q, want 1.5.0", r.DetectorVersion)
	}
	if r.Inputs.Track.ArtistName != "Det Artist" || r.Inputs.Track.TrackName != "Det Title" {
		t.Errorf("track = %q/%q, want Det Artist/Det Title", r.Inputs.Track.ArtistName, r.Inputs.Track.TrackName)
	}
	if r.Inputs.Outdir != "out" || r.Inputs.Filename != "a.lrc" {
		t.Errorf("outdir/filename = %q/%q, want out/a.lrc", r.Inputs.Outdir, r.Inputs.Filename)
	}
	// OutputPaths hydrates from the empty output_paths column via the (outdir, filename) fallback.
	if len(r.Inputs.OutputPaths) != 1 || r.Inputs.OutputPaths[0].Outdir != "out" || r.Inputs.OutputPaths[0].Filename != "a.lrc" {
		t.Errorf("OutputPaths = %+v, want one {out, a.lrc}", r.Inputs.OutputPaths)
	}
}

// markDetectorInstrumental flips an existing work_queue row into the
// detector-instrumental shape ListDetectorInstrumentalMarkers selects on
// (instrumental_result=1, status='done').
func markDetectorInstrumental(t *testing.T, q *DBQueue, id int64, detectorVersion string) {
	t.Helper()
	if _, err := q.db.ExecContext(context.Background(),
		`UPDATE work_queue SET instrumental_result=1, status='done', outcome_type='instrumental', detector_version=? WHERE id=?`,
		detectorVersion, id); err != nil {
		t.Fatalf("mark detector-instrumental: %v", err)
	}
}

// TestListDetectorInstrumentalMarkers_ScopesToLibrary verifies the --library
// narrowing filters the candidate set, mirroring
// TestListVocalGateRejections_ScopesToLibrary.
func TestListDetectorInstrumentalMarkers_ScopesToLibrary(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	libA, srA := insertLibraryAndScanResult(t, sqlDB, "/libA", "/libA/a.mp3")
	_, srB := insertLibraryAndScanResult(t, sqlDB, "/libB", "/libB/b.mp3")

	mk := func(title, src string, scanResultID int64) int64 {
		item, err := q.Enqueue(ctx, models.Inputs{
			Track:        models.Track{ArtistName: "Artist", TrackName: title},
			Outdir:       "out",
			Filename:     title + ".lrc",
			SourcePath:   src,
			ScanResultID: scanResultID,
		}, PriorityScan)
		if err != nil {
			t.Fatalf("enqueue %s: %v", title, err)
		}
		if _, err := q.Dequeue(ctx); err != nil {
			t.Fatalf("dequeue %s: %v", title, err)
		}
		if _, err := q.Defer(ctx, item.ID, 0, nil); err != nil {
			t.Fatalf("defer %s: %v", title, err)
		}
		markDetectorInstrumental(t, q, item.ID, "1.5.0")
		return item.ID
	}
	inA := mk("in-lib-a", "/libA/a.mp3", srA)
	_ = mk("in-lib-b", "/libB/b.mp3", srB)

	got, err := q.ListDetectorInstrumentalMarkers(ctx, ListInstrumentalMarkersOptions{LibraryID: &libA})
	if err != nil {
		t.Fatalf("ListDetectorInstrumentalMarkers: %v", err)
	}
	if len(got) != 1 || got[0].ID != inA {
		var ids []int64
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		t.Fatalf("scoped list = %v; want only the libA row %d", ids, inA)
	}
}

// TestListDetectorInstrumentalMarkers_Limit verifies the Limit option caps
// the result set, and that LibraryID=nil returns all rows when unset.
func TestListDetectorInstrumentalMarkers_Limit(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	for i := 0; i < 2; i++ {
		id := seedDeferredRow(t, q, "Artist", fmt.Sprintf("Title%d", i), fmt.Sprintf("/music/%d.flac", i))
		markDetectorInstrumental(t, q, id, "1.0.0")
	}

	all, err := q.ListDetectorInstrumentalMarkers(ctx, ListInstrumentalMarkersOptions{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d; want 2 (nil LibraryID must not filter)", len(all))
	}

	limited, err := q.ListDetectorInstrumentalMarkers(ctx, ListInstrumentalMarkersOptions{Limit: 1})
	if err != nil {
		t.Fatalf("list limited: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("len(limited) = %d; want 1 (Limit must cap the result set)", len(limited))
	}
}

// TestListDetectorInstrumentalMarkers_MalformedOutputPathsErrors verifies a
// row whose stored output_paths column holds malformed JSON (a real form of
// data corruption, not a fault-injected I/O failure) surfaces a clean error
// rather than a panic or a silently wrong row.
func TestListDetectorInstrumentalMarkers_MalformedOutputPathsErrors(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	id := seedDeferredRow(t, q, "Artist", "Title", "/music/a.flac")
	markDetectorInstrumental(t, q, id, "1.0.0")
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET output_paths='not-json' WHERE id=?`, id); err != nil {
		t.Fatalf("corrupt output_paths: %v", err)
	}

	if _, err := q.ListDetectorInstrumentalMarkers(ctx, ListInstrumentalMarkersOptions{}); err == nil {
		t.Fatal("expected an error for malformed output_paths JSON, got nil")
	}
}

// TestListDetectorInstrumentalMarkers_ClosedDBErrors verifies the query error
// path is wrapped cleanly (not panicked) when the underlying connection is
// already closed, a real failure mode distinct from the malformed-JSON scan
// error covered above.
func TestListDetectorInstrumentalMarkers_ClosedDBErrors(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	id := seedDeferredRow(t, q, "Artist", "Title", "/music/a.flac")
	markDetectorInstrumental(t, q, id, "1.0.0")

	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if _, err := q.ListDetectorInstrumentalMarkers(ctx, ListInstrumentalMarkersOptions{}); err == nil {
		t.Fatal("expected an error querying a closed database, got nil")
	}
}

// TestListVocalGateRejections_RequiresGateComplete pins the #511 bit: a row
// stamped from a degraded classifier response (baselineComplete=false: a real
// vocal_peak and a non-empty vocal_class, so every other guard passes) must not
// be listed, a complete row must, and a pre-migration NULL row is excluded.
func TestListVocalGateRejections_RequiresGateComplete(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	tel := InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0"}
	degraded := seedDeferredRow(t, q, "Artist", "Degraded", "/music/degraded.flac")
	complete := seedDeferredRow(t, q, "Artist", "Complete", "/music/complete.flac")
	legacy := seedDeferredRow(t, q, "Artist", "Legacy", "/music/legacy.flac")
	if _, err := q.StampUnclassifiedMiss(ctx, degraded, tel); err != nil { // GateComplete false
		t.Fatalf("stamp degraded: %v", err)
	}
	tel.GateComplete = true
	if _, err := q.StampUnclassifiedMiss(ctx, complete, tel); err != nil {
		t.Fatalf("stamp complete: %v", err)
	}
	tel.GateComplete = false
	if _, err := q.StampUnclassifiedMiss(ctx, legacy, tel); err != nil {
		t.Fatalf("stamp legacy: %v", err)
	}
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET gate_complete = NULL WHERE id = ?`, legacy); err != nil {
		t.Fatalf("null the legacy bit: %v", err)
	}
	var stored int
	if err := q.db.QueryRowContext(ctx, `SELECT gate_complete FROM work_queue WHERE id = ?`, degraded).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("degraded gate_complete = %d, %v; want a stored 0", stored, err)
	}
	got, err := q.ListVocalGateRejections(ctx, ListVocalGateRejectionsOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != complete || !got[0].Tel.GateComplete {
		t.Fatalf("listed %+v; want only the complete row %d", got, complete)
	}
}

// TestGateCompleteWritersAndReset checks every telemetry writer persists the bit
// and ResetInstrumental clears it with the other telemetry columns.
func TestGateCompleteWritersAndReset(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	read := func(id int64) *int {
		t.Helper()
		var v *int
		if err := q.db.QueryRowContext(ctx, `SELECT gate_complete FROM work_queue WHERE id = ?`, id).Scan(&v); err != nil {
			t.Fatalf("read: %v", err)
		}
		return v
	}
	tel := InstrumentalTelemetry{MusicSum: 0.9, VocalPeak: 0.02, SpeechMean: 0.01, VocalClass: "Singing", DetectorVersion: "v1", GateComplete: true}

	settled := seedDeferredRow(t, q, "A", "Settle", "/music/s.flac")
	set := seedDeferredRow(t, q, "A", "Set", "/music/set.flac")
	stamp := seedDeferredRow(t, q, "A", "Stamp", "/music/st.flac")
	if out, err := q.SettleInstrumental(ctx, settled, tel, OwnedByBackfill); err != nil || out != Settled {
		t.Fatalf("settle = %v, %v", out, err)
	}
	if v := read(settled); v == nil || *v != 1 {
		t.Errorf("SettleInstrumental gate_complete = %v; want 1", v)
	}
	if err := q.SetInstrumentalResult(ctx, set, 0, tel); err != nil {
		t.Fatalf("set: %v", err)
	}
	if v := read(set); v == nil || *v != 1 {
		t.Errorf("SetInstrumentalResult gate_complete = %v; want 1", v)
	}

	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET instrumental_result = 1, status = 'done' WHERE id = ?`, stamp); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if ok, err := q.StampInstrumentalTelemetry(ctx, stamp, tel); err != nil || !ok {
		t.Fatalf("stamp telemetry = %v, %v", ok, err)
	}
	if v := read(stamp); v == nil || *v != 1 {
		t.Errorf("StampInstrumentalTelemetry gate_complete = %v; want 1", v)
	}

	if n, err := q.ResetInstrumental(ctx, settled); err != nil || n != 1 {
		t.Fatalf("reset = %d, %v", n, err)
	}
	if v := read(settled); v != nil {
		t.Errorf("ResetInstrumental left gate_complete = %d; want NULL", *v)
	}
}

// StampInstrumentalTelemetry stores the bit it is given, including false: it has
// its own UPDATE text, distinct from StampUnclassifiedMiss, so a stuck-at-true
// there is caught by nothing else (#511).
func TestStampInstrumentalTelemetry_StoresFalseGateBit(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	id := seedDeferredRow(t, q, "A", "Stamp", "/music/st.flac")
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET instrumental_result = 1, status = 'done' WHERE id = ?`, id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tel := InstrumentalTelemetry{MusicSum: 0.9, VocalPeak: 0.02, SpeechMean: 0.01, VocalClass: "Singing", DetectorVersion: "v1"}
	if ok, err := q.StampInstrumentalTelemetry(ctx, id, tel); err != nil || !ok {
		t.Fatalf("stamp = %v, %v", ok, err)
	}
	var v *int
	if err := q.db.QueryRowContext(ctx, `SELECT gate_complete FROM work_queue WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("read: %v", err)
	}
	if v == nil || *v != 0 {
		t.Errorf("gate_complete = %v; want a stored 0 for a degraded stamp", v)
	}
}

// The dequeued WorkItem carries the persisted bit: nil for NULL, pointers to the
// stored value otherwise. The worker memo reuses stored scores only on true.
func TestDequeueCarriesGateCompleteBit(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  string
		want *bool
	}{
		{"null", "NULL", nil},
		{"zero", "0", boolRef(false)},
		{"one", "1", boolRef(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := NewDBQueue(openQueueTestDB(t))
			ctx := context.Background()
			id := seedDeferredRow(t, q, "A", "Carry", "/music/c.flac")
			if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET gate_complete = `+tc.set+`, next_attempt_at = '2000-01-01T00:00:00Z' WHERE id = ?`, id); err != nil {
				t.Fatalf("seed: %v", err)
			}
			item, err := q.Dequeue(ctx)
			if err != nil {
				t.Fatalf("Dequeue: %v", err)
			}
			switch {
			case tc.want == nil && item.DetectorGateComplete != nil:
				t.Errorf("DetectorGateComplete = %v; want nil", *item.DetectorGateComplete)
			case tc.want != nil && (item.DetectorGateComplete == nil || *item.DetectorGateComplete != *tc.want):
				t.Errorf("DetectorGateComplete = %v; want %v", item.DetectorGateComplete, *tc.want)
			}
		})
	}
}

func boolRef(b bool) *bool { return &b }

// CountVocalGateExcluded counts exactly the rows the reopen drops for want of the
// bit (NULL or 0), and nothing the reopen lists.
func TestCountVocalGateExcluded(t *testing.T) {
	q := NewDBQueue(openQueueTestDB(t))
	ctx := context.Background()
	tel := InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0"}
	if n, err := q.CountVocalGateExcluded(ctx, nil); err != nil || n != 0 {
		t.Fatalf("empty queue count = %d, %v; want 0", n, err)
	}
	complete := seedDeferredRow(t, q, "A", "Complete", "/music/complete.flac")
	zero := seedDeferredRow(t, q, "A", "Zero", "/music/zero.flac")
	null := seedDeferredRow(t, q, "A", "Null", "/music/null.flac")
	for id, gate := range map[int64]bool{complete: true, zero: false, null: false} {
		tel.GateComplete = gate
		if _, err := q.StampUnclassifiedMiss(ctx, id, tel); err != nil {
			t.Fatalf("stamp %d: %v", id, err)
		}
	}
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET gate_complete = NULL WHERE id = ?`, null); err != nil {
		t.Fatalf("null the bit: %v", err)
	}
	if n, err := q.CountVocalGateExcluded(ctx, nil); err != nil || n != 2 {
		t.Errorf("count = %d, %v; want 2 (the 0 and the NULL row, not the complete one)", n, err)
	}

	// Rows the list also excludes for another reason must not be counted, even
	// with the gate bit unset: this pins the shared base predicate and the
	// word-recheck exclusion (a count of the gate term alone would include them).
	for _, tc := range []struct{ name, set string }{
		{"done", `status = 'done'`},
		{"null-score", `music_sum = NULL`},
		{"empty-vocal-class", `vocal_class = ''`},
		{"word-recheck", `word_timing_state = 'queued'`},
	} {
		id := seedDeferredRow(t, q, "A", "Other-"+tc.name, "/music/other-"+tc.name+".flac")
		tel.GateComplete = false
		if _, err := q.StampUnclassifiedMiss(ctx, id, tel); err != nil {
			t.Fatalf("stamp %s: %v", tc.name, err)
		}
		if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET gate_complete = NULL, `+tc.set+` WHERE id = ?`, id); err != nil { //nolint:gosec // reason: G202 -- tc.set is a literal from this test table
			t.Fatalf("make %s row: %v", tc.name, err)
		}
	}
	if n, err := q.CountVocalGateExcluded(ctx, nil); err != nil || n != 2 {
		t.Errorf("count with other-reason rows = %d, %v; want still 2", n, err)
	}
}

// CountVocalGateExcluded honors the same library option the list does.
func TestCountVocalGateExcluded_ScopesToLibrary(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	libA, srA := insertLibraryAndScanResult(t, sqlDB, "/libA", "/libA/a.mp3")
	libB, srB := insertLibraryAndScanResult(t, sqlDB, "/libB", "/libB/b.mp3")
	tel := InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0"}
	for i, sr := range []int64{srA, srB, srB} {
		item, err := q.Enqueue(ctx, models.Inputs{
			Track:        models.Track{ArtistName: "Artist", TrackName: fmt.Sprintf("t%d", i)},
			Outdir:       "out",
			Filename:     fmt.Sprintf("t%d.lrc", i),
			SourcePath:   fmt.Sprintf("/lib/t%d.mp3", i),
			ScanResultID: sr,
		}, PriorityScan)
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if _, err := q.Dequeue(ctx); err != nil {
			t.Fatalf("dequeue %d: %v", i, err)
		}
		if _, err := q.Defer(ctx, item.ID, time.Hour, errors.New("no results found")); err != nil {
			t.Fatalf("defer %d: %v", i, err)
		}
		if _, err := q.StampUnclassifiedMiss(ctx, item.ID, tel); err != nil {
			t.Fatalf("stamp %d: %v", i, err)
		}
	}
	for _, tc := range []struct {
		name string
		lib  *int64
		want int
	}{{"all", nil, 3}, {"libA", &libA, 1}, {"libB", &libB, 2}} {
		if n, err := q.CountVocalGateExcluded(ctx, tc.lib); err != nil || n != tc.want {
			t.Errorf("%s: count = %d, %v; want %d", tc.name, n, err, tc.want)
		}
	}
}
