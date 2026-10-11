package worker

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/orchestrator"
)

func gateBit(t *testing.T, sqlDB *sql.DB, title string) (status string, result, gate *int) {
	t.Helper()
	if err := sqlDB.QueryRowContext(context.Background(),
		`SELECT status, instrumental_result, gate_complete FROM work_queue WHERE title = ?`, title,
	).Scan(&status, &result, &gate); err != nil {
		t.Fatalf("read %q: %v", title, err)
	}
	return status, result, gate
}

// A row stamped before migration 070 (stored scores and a matching version, NULL
// gate bit) must be re-detected ONCE, stamped gate-complete, and then reused with
// no further detector call (#511). Reuse of the NULL row would re-decide it
// instrumental through DecideStored with the completeness guards skipped, and
// would never stamp the bit. This also pins the live stamp propagation
// (stampDetectorMissTelemetry) end to end through the real queue.
func TestRun_NullGateRowRedetectedOnceThenReused(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, orchestrator.ModeOrdered, http.StatusNotFound)
	det := &fakeStoredDecider{
		version: "v1",
		detectRes: detector.Result{Version: "v1", Confidence: 0.95, VocalConfidence: 0.01, SpeechConfidence: 0.01,
			WinningVocalClass: "Singing", Reusable: true},
	}
	rig.w.EnableAudioDetector(det)
	rig.w.SetInstrumentalDetectionDefault(true)
	rig.enqueue("Premigration")
	if _, err := rig.db.ExecContext(ctx, `UPDATE work_queue SET instrumental_result = 0, music_sum = 0.95, vocal_peak = 0.01,
		speech_mean = 0.01, vocal_class = 'Singing', detector_version = 'v1', gate_complete = NULL WHERE title = 'Premigration'`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := rig.w.Run(ctx); err != nil {
		t.Fatalf("Run pass 1: %v", err)
	}
	if det.detectN != 1 {
		t.Fatalf("detector calls after pass 1 = %d; want 1 (NULL bit forces a live detection)", det.detectN)
	}
	if status, _, gate := gateBit(t, rig.db, "Premigration"); status != "deferred" || gate == nil || *gate != 1 {
		t.Fatalf("after pass 1 status=%q gate_complete=%v; want deferred and 1", status, gate)
	}

	rig.now = rig.now.Add(48 * time.Hour)
	if err := rig.w.Run(ctx); err != nil {
		t.Fatalf("Run pass 2: %v", err)
	}
	if det.detectN != 1 {
		t.Errorf("detector calls after pass 2 = %d; want still 1 (the re-detection is one-time, then the memo reuses)", det.detectN)
	}
	if _, _, gate := gateBit(t, rig.db, "Premigration"); gate == nil || *gate != 1 {
		t.Errorf("gate_complete after the reuse pass = %v; want 1", gate)
	}
}

// A degraded row (gate_complete = 0) with scores that would settle instrumental
// if re-decided must not settle without a fresh detection.
func TestRun_DegradedRowIsNotSettledFromStoredScores(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, orchestrator.ModeOrdered, http.StatusNotFound)
	det := &fakeStoredDecider{
		version:   "v1",
		detectRes: detector.Result{Version: "v1", Confidence: 0.95, Reusable: false},
		decideFn:  func(_, _, _ float64) bool { return true },
	}
	rig.w.EnableAudioDetector(det)
	rig.w.SetInstrumentalDetectionDefault(true)
	rig.enqueue("Degraded")
	if _, err := rig.db.ExecContext(ctx, `UPDATE work_queue SET instrumental_result = 0, music_sum = 0.97, vocal_peak = 0,
		speech_mean = 0.001, vocal_class = '', detector_version = 'v1', gate_complete = 0 WHERE title = 'Degraded'`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := rig.w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	status, result, _ := gateBit(t, rig.db, "Degraded")
	if det.detectN != 1 {
		t.Errorf("detector calls = %d; want 1 (live detection, not a reuse)", det.detectN)
	}
	if status == "done" || (result != nil && *result == 1) {
		t.Errorf("degraded row settled instrumental (status=%q result=%v) from stored scores", status, result)
	}
}

// A live complete instrumental detection must reach the row with the bit set:
// detector lane (DetectorGateComplete) -> settleDetectorInstrumental -> queue.
func TestRun_LiveInstrumentalSettleStampsGateComplete(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, orchestrator.ModeOrdered, http.StatusNotFound)
	det := &fakeStoredDecider{
		version: "v1",
		detectRes: detector.Result{Instrumental: true, Version: "v1", Confidence: 0.95, VocalConfidence: 0.01,
			SpeechConfidence: 0.01, WinningVocalClass: "Singing", Reusable: true},
	}
	rig.w.EnableAudioDetector(det)
	rig.w.SetInstrumentalDetectionDefault(true)
	rig.enqueue("Interlude")

	if err := rig.w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	status, result, gate := gateBit(t, rig.db, "Interlude")
	if status != "done" || result == nil || *result != 1 {
		t.Fatalf("status=%q result=%v; want a detector instrumental settle", status, result)
	}
	if gate == nil || *gate != 1 {
		t.Errorf("gate_complete = %v; want 1 on a settle from a complete response", gate)
	}
}
