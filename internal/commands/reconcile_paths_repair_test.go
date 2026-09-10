package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// seedReconcilePathsPreBrokenRow reproduces the live-production shape #921
// describes at the CLI layer: a work_queue row whose outdir/filename/
// source_path already point at the file's REAL, current location (a prior
// relink already moved them) while output_paths still names the vanished
// pre-move directory. No scan_results row exists for the stale location --
// mirroring production, where the relink's own cleanup already deleted it --
// so this row is invisible to the sweep half of reconcile-paths and can only
// be reached by the repair pass.
func seedReconcilePathsPreBrokenRow(t *testing.T, ctx context.Context, dbPath, newPath, staleDir string) int64 {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename, recording_mbid) VALUES (1, ?, 'Artist', 'Title', 'done', ?, ?, ?)`,
		newPath, filepath.Dir(newPath), filepath.Base(newPath), "mbid-921-cli")
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
		Track:      models.Track{ArtistName: "Artist", TrackName: filepath.Base(newPath)},
		Outdir:     filepath.Dir(newPath),
		Filename:   filepath.Base(newPath),
		SourcePath: newPath,
		OutputPaths: []models.OutputPath{{
			Outdir:   staleDir,
			Filename: filepath.Base(newPath),
		}},
		ScanResultID: srID,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'failed' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	return item.ID
}

// reconcilePathsWorkQueueOutputPaths reads back and decodes one work_queue
// row's output_paths column by id.
func reconcilePathsWorkQueueOutputPaths(t *testing.T, ctx context.Context, dbPath string, id int64) []models.OutputPath {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
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

// TestReconcilePaths_RepairDryRunReportsWithoutMutating: the output_paths
// repair pass (#921) previews under dry-run without writing, matching the
// sweep half's ergonomics, and its summary line is aggregate-only -- no path,
// artist, or title -- per the reconcile family's privacy convention.
func TestReconcilePaths_RepairDryRunReportsWithoutMutating(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	newPath := filepath.Join(root, "Artist921CLI", "AlbumNew", "01. track.flac")
	staleDir := filepath.Join(root, "Artist921CLI", "AlbumOld")
	wqID := seedReconcilePathsPreBrokenRow(t, ctx, dbPath, newPath, staleDir)
	// A second row with malformed output_paths pins that skip counts reach stdout.
	// Its source_path is emptied because Sweep's gather (which runs first) fails
	// loudly on malformed output_paths for any row it selects by source_path.
	badID := seedReconcilePathsPreBrokenRow(t, ctx, dbPath, filepath.Join(root, "Bad", "New", "02.flac"), filepath.Join(root, "Bad", "Old"))
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET output_paths = 'not json', source_path = '' WHERE id = ?`, badID); err != nil {
		t.Fatalf("malform row: %v", err)
	}
	_ = sqlDB.Close()

	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "would repair 1 work_queue row") ||
		!strings.Contains(buf.String(), "(skipped: 0 ambiguous, 0 unfixable, 0 stat error, 1 malformed, 0 raced)") {
		t.Errorf("want 'would repair 1 work_queue row' with every skip count; got: %s", buf.String())
	}
	// Privacy convention (repo CLAUDE.md / commands.go doc on the reconcile
	// family): aggregate-only stdout, never a path/artist/title.
	if strings.Contains(buf.String(), root) || strings.Contains(buf.String(), "Artist921CLI") {
		t.Errorf("summary line leaked a path/identifier; got: %s", buf.String())
	}

	got := reconcilePathsWorkQueueOutputPaths(t, ctx, dbPath, wqID)
	if len(got) != 1 || got[0].Outdir != staleDir {
		t.Errorf("dry run mutated output_paths: got %+v, want the stale entry unchanged", got)
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "reconcile-paths-backup-*.jsonl")); len(matches) != 0 {
		t.Errorf("dry-run wrote a backup: %v", matches)
	}
}

// TestReconcilePaths_RepairApplyFixesRowAndBacksUp: --yes rewrites the stale
// output_paths entry to the row's own (already-correct) outdir/filename and
// records a "repaired" backup line an operator can use to restore the prior
// value.
func TestReconcilePaths_RepairApplyFixesRowAndBacksUp(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	newPath := filepath.Join(root, "Artist921CLI2", "AlbumNew", "01. track.flac")
	staleDir := filepath.Join(root, "Artist921CLI2", "AlbumOld")
	wqID := seedReconcilePathsPreBrokenRow(t, ctx, dbPath, newPath, staleDir)

	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "repaired 1 work_queue row") || !strings.Contains(buf.String(), "retained/repaired rows written to") {
		t.Errorf("want 'repaired 1 work_queue row' and a backup line naming repaired rows; got: %s", buf.String())
	}

	got := reconcilePathsWorkQueueOutputPaths(t, ctx, dbPath, wqID)
	want := models.OutputPath{Outdir: filepath.Dir(newPath), Filename: filepath.Base(newPath)}
	if len(got) != 1 || got[0] != want {
		t.Errorf("output_paths after repair = %+v, want [%+v]", got, want)
	}

	recs := readReconcilePathsBackup(t, filepath.Dir(dbPath))
	var repairedRecs []reconcilePathsBackupRecord
	for _, r := range recs {
		if r.Action == "repaired" {
			repairedRecs = append(repairedRecs, r)
		}
	}
	if len(repairedRecs) != 1 {
		t.Fatalf("backup holds %d 'repaired' records, want exactly 1: %+v", len(repairedRecs), recs)
	}
	rec := repairedRecs[0]
	if rec.WorkItemID != wqID {
		t.Errorf("backup record WorkItemID = %d, want %d", rec.WorkItemID, wqID)
	}
	if len(rec.OldOutputPaths) != 1 || rec.OldOutputPaths[0].Outdir != staleDir {
		t.Errorf("backup record OldOutputPaths = %+v, want the stale entry preserved for restore", rec.OldOutputPaths)
	}
	if len(rec.NewOutputPaths) != 1 || rec.NewOutputPaths[0] != want {
		t.Errorf("backup record NewOutputPaths = %+v, want [%+v]", rec.NewOutputPaths, want)
	}
}
