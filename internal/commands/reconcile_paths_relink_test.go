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

// seedReconcilePathsRowWithIdentity is seedReconcilePathsRow plus an explicit
// recording_mbid, so the caller controls whether the seeded row is a relink
// candidate (identity shared with a present file), a retain candidate
// (identity absent), or a genuine-delete candidate (identity present but
// unique/unmatched) -- the three outcomes runReconcilePaths must now report
// distinctly (#640).
func seedReconcilePathsRowWithIdentity(t *testing.T, ctx context.Context, dbPath, filePath, mbid string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, recording_mbid) VALUES (1, ?, 'Artist', 'Title', 'processing', ?)`,
		filePath, mbid)
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
		Track:        models.Track{ArtistName: "Artist", TrackName: filepath.Base(filePath)},
		SourcePath:   filePath,
		OutputPaths:  []models.OutputPath{{Outdir: filepath.Dir(filePath), Filename: filepath.Base(filePath)}},
		ScanResultID: srID,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Mimic the wedged state the ticket targets: a failed queue row.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'failed' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("set failed: %v", err)
	}
}

// seedReconcilePathsPresentFile inserts a bare, unlinked scan_results row for
// a file that already exists on disk, carrying mbid -- the present-file
// candidate side of a relink match (as if a rescan had already discovered the
// file at its new location).
func seedReconcilePathsPresentFile(t *testing.T, ctx context.Context, dbPath, filePath, mbid string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write present source: %v", err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed present: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename, recording_mbid)
         VALUES (1, ?, 'Artist', 'Title', 'pending', ?, ?, ?)`,
		filePath, filepath.Dir(filePath), filepath.Base(filePath), mbid,
	); err != nil {
		t.Fatalf("insert present scan_result: %v", err)
	}
}

// TestReconcilePaths_RelinkDryRunReportsWithoutMutating: a gone row whose
// identity resolves uniquely to a present file elsewhere in the library is
// reported as a relink candidate in the summary line, with the database left
// untouched and no backup written (dry-run by default, matching the delete
// path's existing ergonomics).
func TestReconcilePaths_RelinkDryRunReportsWithoutMutating(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	oldPath := filepath.Join(root, "ArtistM", "AlbumOld", "01. track.flac")
	newPath := filepath.Join(root, "ArtistM", "AlbumNew", "01. track.flac")
	seedReconcilePathsRowWithIdentity(t, ctx, dbPath, oldPath, "mbid-m-shared")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	seedReconcilePathsPresentFile(t, ctx, dbPath, newPath, "mbid-m-shared")

	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "would relink 1 source") {
		t.Errorf("want 'would relink 1 source'; got: %s", buf.String())
	}
	if n := countRows(t, ctx, dbPath, "scan_results"); n != 2 {
		t.Errorf("dry-run mutated scan_results: n=%d, want 2 (both survive)", n)
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "reconcile-paths-backup-*.jsonl")); len(matches) != 0 {
		t.Errorf("dry-run wrote a backup: %v", matches)
	}
}

// TestReconcilePaths_RelinkApplyUpdatesSourceAndBacksUpWithAction: --yes
// performs the relink (moving the surviving work_queue row's source_path to
// the new location and deleting the stale scan_results row), and the JSONL
// backup record for it carries action="relinked" plus the old/new paths and
// the identity that drove the match -- the shape the CLI docstring promises.
func TestReconcilePaths_RelinkApplyUpdatesSourceAndBacksUpWithAction(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	oldPath := filepath.Join(root, "ArtistN", "AlbumOld", "01. track.flac")
	newPath := filepath.Join(root, "ArtistN", "AlbumNew", "01. track.flac")
	seedReconcilePathsRowWithIdentity(t, ctx, dbPath, oldPath, "mbid-n-shared")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	seedReconcilePathsPresentFile(t, ctx, dbPath, newPath, "mbid-n-shared")

	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "relinked 1 source") {
		t.Errorf("want 'relinked 1 source'; got: %s", buf.String())
	}
	// Exactly one scan_results row remains (the present one, now the sole
	// owner); the stale gone-path row was removed by the relink.
	if n := countRows(t, ctx, dbPath, "scan_results"); n != 1 {
		t.Errorf("after relink scan_results=%d, want 1", n)
	}

	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "reconcile-paths-backup-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want exactly one backup file; got %v", matches)
	}
	b, err := os.ReadFile(matches[0]) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var rec reconcilePathsBackupRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("decode backup: %v", err)
	}
	if rec.Action != "relinked" || rec.SourcePath != oldPath || rec.NewPath != newPath || rec.MBID != "mbid-n-shared" {
		t.Errorf("backup record = %+v; want action=relinked source=%q new=%q mbid=mbid-n-shared", rec, oldPath, newPath)
	}
}

// TestReconcilePaths_RetainedReportsAndBacksUpWithReason: a gone row with no
// stored identity is neither pruned nor relinked -- it is retained, and under
// --yes the backup captures the reason so an operator understands why the row
// survives.
func TestReconcilePaths_RetainedReportsAndBacksUpWithReason(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	gone := filepath.Join(root, "ArtistO", "01. gone.flac")
	// No identity: mbid="" is the "never enriched" sentinel scan_results uses.
	seedReconcilePathsRowWithIdentity(t, ctx, dbPath, gone, "")
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}

	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "retained 1 source") {
		t.Errorf("want 'retained 1 source'; got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "pruned 0 source") {
		t.Errorf("want 'pruned 0 source' (identity-absent must not be deleted); got: %s", buf.String())
	}
	if n := countRows(t, ctx, dbPath, "scan_results"); n != 1 {
		t.Errorf("retained row was mutated: scan_results=%d, want 1 (kept)", n)
	}

	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "reconcile-paths-backup-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want exactly one backup file; got %v", matches)
	}
	b, err := os.ReadFile(matches[0]) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var rec reconcilePathsBackupRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("decode backup: %v", err)
	}
	if rec.Action != "retained" || rec.SourcePath != gone || rec.Reason == "" {
		t.Errorf("backup record = %+v; want action=retained source=%q with a non-empty reason", rec, gone)
	}
}

// TestReconcilePaths_BackupOpenFailureFailsTheRun: when the backup path
// cannot be opened for writing (its parent directory does not exist), the
// command surfaces the failure via a non-zero exit rather than silently
// dropping the record -- the backup-first invariant means a report failure
// must abort, not proceed unrecorded.
func TestReconcilePaths_BackupOpenFailureFailsTheRun(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	gone := filepath.Join(root, "ArtistP", "01. gone.flac")
	seedReconcilePathsRow(t, ctx, dbPath, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}

	badBackup := filepath.Join(root, "no-such-parent-dir", "backup.jsonl")
	var buf bytes.Buffer
	code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true, Backup: badBackup})
	if code != 1 {
		t.Fatalf("exit=%d, want 1 for an unopenable backup path; out=%s", code, buf.String())
	}
}

// TestReconcilePaths_ScopedRelinkOnlyMatchesWithinLibrary: --library scoping
// narrows the present-file candidate pool the same way it narrows the gone
// candidates, exercising the SweepOptions.LibraryID wiring on the relink path
// specifically (the delete path's library scoping is already covered by
// TestReconcilePaths_LibraryScoped).
func TestReconcilePaths_ScopedRelinkOnlyMatchesWithinLibrary(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	oldPath := filepath.Join(root, "ArtistQ", "AlbumOld", "01. track.flac")
	newPath := filepath.Join(root, "ArtistQ", "AlbumNew", "01. track.flac")
	seedReconcilePathsRowWithIdentity(t, ctx, dbPath, oldPath, "mbid-q-shared")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	seedReconcilePathsPresentFile(t, ctx, dbPath, newPath, "mbid-q-shared")

	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Library: "lib", Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "relinked 1 source") {
		t.Errorf("want 'relinked 1 source'; got: %s", buf.String())
	}
}
