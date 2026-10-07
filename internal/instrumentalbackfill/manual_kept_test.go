package instrumentalbackfill

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// A manual instrumental marker on disk is itself the instrumental verdict: the
// refusal must settle the row instrumental (leaving the candidate listing),
// never stamp it not-instrumental, and never rewrite the marker.
func TestRun_ManualMarkerKeptSettlesInstrumental(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	q := queue.NewDBQueue(sqlDB)
	lib := t.TempDir()
	txt := filepath.Join(lib, "track.txt")
	body := []byte("[by:canticle]\n[source:manual]\n" + lyrics.InstrumentalMarker + "\n")
	if err := os.WriteFile(txt, body, 0o600); err != nil {
		t.Fatal(err)
	}
	it, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"},
		Outdir:     lib,
		Filename:   "track.lrc",
		SourcePath: filepath.Join(lib, "track.flac"),
	}, queue.PriorityScan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Defer(ctx, it.ID, time.Hour, errors.New("no results found")); err != nil {
		t.Fatal(err)
	}

	det := &countingDetector{res: detector.Result{Instrumental: true, Confidence: 0.95, Version: "v1"}}
	bf := New(q, det, lyrics.NewLRCWriter(lib))
	for cycle := 1; cycle <= 2; cycle++ {
		if _, err := bf.Run(ctx, Options{GlobalDetectDefault: true}); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	if det.calls != 1 {
		t.Fatalf("detector calls = %d over 2 cycles; want 1", det.calls)
	}
	if got, err := os.ReadFile(txt); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("manual marker changed: %q, %v", got, err)
	}
	var status string
	var result sql.NullInt64
	if err := sqlDB.QueryRow(`SELECT status, instrumental_result FROM work_queue WHERE id = ?`, it.ID).Scan(&status, &result); err != nil {
		t.Fatal(err)
	}
	if status != "done" || !result.Valid || result.Int64 != 1 {
		t.Fatalf("row = %s/%v; want done with instrumental_result=1", status, result)
	}
}
