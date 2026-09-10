package prune

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// seedRowWithOutputPaths is seedRowWithIdentity, but lets the caller supply
// the row's output_paths directly instead of the single-entry default
// queue.Enqueue derives from Outdir/Filename -- needed to exercise a
// work_queue row whose output_paths already carries more than one entry (the
// shape identityrepair.mergeQueueRows produces when two rows collide on
// (artist_key, title_key) and their output_paths are unioned). mbid doubles as
// the title so several rows can coexist under UNIQUE(artist_key, title_key).
func seedRowWithOutputPaths(t *testing.T, ctx context.Context, sqlDB *sql.DB, libraryID int64, filePath, mbid string, outputPaths []models.OutputPath) int64 {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename, recording_mbid) VALUES (?, ?, ?, ?, 'done', ?, ?, ?)`,
		libraryID, filePath, "Artist", mbid, filepath.Dir(filePath), filepath.Base(filePath), mbid)
	if err != nil {
		t.Fatalf("insert scan_result: %v", err)
	}
	srID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("scan_result id: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: mbid},
		Outdir:       filepath.Dir(filePath),
		Filename:     filepath.Base(filePath),
		SourcePath:   filePath,
		OutputPaths:  outputPaths,
		ScanResultID: srID,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'done' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("set done: %v", err)
	}
	return item.ID
}

// workQueueOutputPaths reads back and decodes a work_queue row's output_paths
// column.
func workQueueOutputPaths(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64) []models.OutputPath {
	t.Helper()
	var raw string
	if err := sqlDB.QueryRowContext(ctx, `SELECT output_paths FROM work_queue WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("query output_paths: %v", err)
	}
	var paths []models.OutputPath
	if err := json.Unmarshal([]byte(raw), &paths); err != nil {
		t.Fatalf("unmarshal output_paths %q: %v", raw, err)
	}
	return paths
}
