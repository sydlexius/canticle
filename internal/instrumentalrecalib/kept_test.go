package instrumentalrecalib

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/queue"
)

// TestRun_KeptBetterLyricsIsNotAnError is the #553 resource finding for the
// recalibrator: a row that now passes the gate but whose synced .lrc is already
// on disk is refused by the real writer. It must keep its terminal
// not-instrumental verdict (never reset to unclassified, which would hand it
// back to the backfill for a fresh inference) and count as kept, not an error,
// on every cycle.
func TestRun_KeptBetterLyricsIsNotAnError(t *testing.T) {
	ctx := context.Background()
	q, sqlDB := openTestQueueWithDB(t)
	lib := t.TempDir()
	lrc := filepath.Join(lib, "violin.lrc")
	original := []byte("[00:01.00]settled line\n")
	if err := os.WriteFile(lrc, original, 0o600); err != nil {
		t.Fatal(err)
	}
	id := seedRejection(t, q, filepath.Join(lib, "violin.flac"), queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.18.0",
	})
	r := New(q, lyrics.NewLRCWriter(lib))
	for cycle := 1; cycle <= 2; cycle++ {
		res, err := r.Run(ctx, Options{MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20, CurrentVersion: "1.18.0"})
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if res.Errors != 0 || res.KeptOnDisk != 1 || res.Settled != 0 || res.MarkersWritten != 0 {
			t.Fatalf("cycle %d: %+v; want one kept row, no error, no settle", cycle, res)
		}
	}
	if got, err := os.ReadFile(lrc); err != nil || !bytes.Equal(got, original) {
		t.Fatalf(".lrc changed: %q, %v", got, err)
	}
	var status string
	var result sql.NullInt64
	if err := sqlDB.QueryRow(`SELECT status, instrumental_result FROM work_queue WHERE id = ?`, id).Scan(&status, &result); err != nil {
		t.Fatal(err)
	}
	if status != "deferred" || !result.Valid || result.Int64 != 0 {
		t.Fatalf("row = %s/%v; want deferred with instrumental_result=0 (terminal, never re-detected)", status, result)
	}
	unclassified, err := q.ListUnclassified(ctx, queue.ListUnclassifiedOptions{GlobalDetectDefault: true})
	if err != nil || len(unclassified) != 0 {
		t.Fatalf("ListUnclassified = %d rows, %v; the kept row must not return to the detector backlog", len(unclassified), err)
	}
}
