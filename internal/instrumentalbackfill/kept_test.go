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

type countingDetector struct {
	res   detector.Result
	calls int
}

func (d *countingDetector) Detect(context.Context, string) (detector.Result, error) {
	d.calls++
	return d.res, nil
}

// TestRun_KeptBetterLyricsLeaveTheBacklog is the #553 resource finding: an
// instrumental verdict for a row whose synced .lrc is already on disk is refused
// by the real writer's no-downgrade guard. The row must leave the candidate set
// with a terminal not-instrumental stamp, not stay unclassified and be
// re-detected (audio read + inference, a disk wake) every cycle forever.
func TestRun_KeptBetterLyricsLeaveTheBacklog(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	q := queue.NewDBQueue(sqlDB)
	lib := t.TempDir()
	lrc := filepath.Join(lib, "track.lrc")
	original := []byte("[00:01.00]settled line\n")
	if err := os.WriteFile(lrc, original, 0o600); err != nil {
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
		res, err := bf.Run(ctx, Options{GlobalDetectDefault: true})
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if res.Errors != 0 {
			t.Fatalf("cycle %d: errors = %d; a kept file is not an error (%+v)", cycle, res.Errors, res)
		}
		if cycle == 1 && (res.KeptOnDisk != 1 || res.RowsStamped != 1 || res.MarkersWritten != 0) {
			t.Fatalf("cycle 1: %+v; want one kept row stamped, no marker", res)
		}
	}
	if det.calls != 1 {
		t.Fatalf("detector calls = %d over 2 cycles; want 1 (the kept row must leave the backlog)", det.calls)
	}
	if got, err := os.ReadFile(lrc); err != nil || !bytes.Equal(got, original) {
		t.Fatalf(".lrc changed: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(lib, "track.txt")); !os.IsNotExist(err) {
		t.Fatalf("a marker landed beside the kept .lrc: %v", err)
	}
	var status string
	var result sql.NullInt64
	if err := sqlDB.QueryRow(`SELECT status, instrumental_result FROM work_queue WHERE id = ?`, it.ID).Scan(&status, &result); err != nil {
		t.Fatal(err)
	}
	if status != "deferred" || !result.Valid || result.Int64 != 0 {
		t.Fatalf("row = %s/%v; want deferred with instrumental_result=0", status, result)
	}
}

// partialThenKeptWriter writes a real marker for the first path, then swaps it
// for a non-empty directory (so removing it fails on every platform and for
// root), and refuses the second path with ErrKeptBetter.
type partialThenKeptWriter struct{ calls int }

func (w *partialThenKeptWriter) WriteLRC(song models.Song, filename, outdir string) error {
	w.calls++
	if w.calls > 1 {
		return lyrics.ErrKeptBetter
	}
	name, err := lyrics.SidecarName(song.Track.ArtistName, song.Track.TrackName, filename, false)
	if err != nil {
		return err
	}
	p := filepath.Join(outdir, name)
	if err := os.MkdirAll(filepath.Join(p, "child"), 0o750); err != nil {
		return err
	}
	return nil
}

// TestRun_KeptBetterWithFailedRollbackDoesNotStamp: when an earlier output path's
// marker cannot be taken back, the row must not be stamped not-instrumental
// beside the surviving marker; it stays unclassified for the next cycle.
func TestRun_KeptBetterWithFailedRollbackDoesNotStamp(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	q := queue.NewDBQueue(sqlDB)
	lib1, lib2 := t.TempDir(), t.TempDir()
	it, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"},
		OutputPaths: []models.OutputPath{
			{Outdir: lib1, Filename: "track.lrc"},
			{Outdir: lib2, Filename: "track.lrc"},
		},
		SourcePath: filepath.Join(lib1, "track.flac"),
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
	var outcomes []Outcome
	bf := New(q, det, &partialThenKeptWriter{})
	res, err := bf.Run(ctx, Options{GlobalDetectDefault: true, Outcome: func(o Outcome) error { outcomes = append(outcomes, o); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != 1 || res.RowsStamped != 0 || res.KeptOnDisk != 0 {
		t.Fatalf("result = %+v; want 1 error, nothing stamped, nothing kept", res)
	}
	if len(outcomes) != 1 || outcomes[0].Status != OutcomeFailed {
		t.Fatalf("outcomes = %+v; want one OutcomeFailed", outcomes)
	}
	var result sql.NullInt64
	if err := sqlDB.QueryRow(`SELECT instrumental_result FROM work_queue WHERE id = ?`, it.ID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if result.Valid {
		t.Fatalf("instrumental_result = %d; the row must stay unclassified beside a surviving marker", result.Int64)
	}
}
