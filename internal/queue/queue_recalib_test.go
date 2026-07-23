package queue

import (
	"context"
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

	cases := []struct {
		name  string
		setup string // applied to the seeded row
		want  bool
	}{
		{"unscored done instrumental", `instrumental_result = 1, status = 'done'`, true},
		{"already scored", `instrumental_result = 1, status = 'done', music_sum = 0.5`, false},
		{"still processing", `instrumental_result = 1, status = 'processing'`, false},
		{"not instrumental", `instrumental_result = 0, status = 'done'`, false},
		{"no verdict", `instrumental_result = NULL, status = 'done'`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := NewDBQueue(openQueueTestDB(t))
			id := seedDeferredRow(t, q, "Artist", "Title", "/music/a.flac")
			if _, err := q.db.ExecContext(ctx,
				fmt.Sprintf(`UPDATE work_queue SET %s WHERE id = ?`, tc.setup), id); err != nil {
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
	if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0"}); err != nil {
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
	if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0"}); err != nil {
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
		if _, err := q.StampUnclassifiedMiss(ctx, item.ID, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0"}); err != nil {
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
		if _, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0"}); err != nil {
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
