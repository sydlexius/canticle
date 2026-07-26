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
	"github.com/sydlexius/canticle/internal/library"
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
	// The DATABASE artifact, not just the summary line: the surviving
	// work_queue row actually points at the new location. A relink that
	// reported success but left source_path on the vanished path would still
	// satisfy the count assertion above.
	if got := reconcilePathsWorkQueueSource(t, ctx, dbPath, oldPath, newPath); got != newPath {
		t.Errorf("work_queue.source_path = %q, want %q", got, newPath)
	}

	// The backup is THE recovery artifact for a destructive path -- an operator
	// undoes a relink by reversing OldPath/NewPath out of it -- so decode every
	// line and assert the whole set, rather than only the first line. A stray
	// extra record, or a missing new_path, makes the file unusable for restore.
	recs := readReconcilePathsBackup(t, filepath.Dir(dbPath))
	if len(recs) != 1 {
		t.Fatalf("backup holds %d records, want exactly 1: %+v", len(recs), recs)
	}
	rec := recs[0]
	if rec.Action != "relinked" || rec.SourcePath != oldPath || rec.NewPath != newPath || rec.MBID != "mbid-n-shared" {
		t.Errorf("backup record = %+v; want action=relinked source=%q new=%q mbid=mbid-n-shared", rec, oldPath, newPath)
	}
	// Both halves of the reversal must be present and distinct, or the record
	// cannot be undone.
	if rec.NewPath == "" || rec.NewPath == rec.SourcePath {
		t.Errorf("backup record is not reversible: source=%q new=%q", rec.SourcePath, rec.NewPath)
	}
	// The work_queue ids are what a hand-restore would target.
	if len(rec.WorkItemIDs) != 1 {
		t.Errorf("backup record work_item_ids = %v, want exactly one id to restore against", rec.WorkItemIDs)
	}
}

// readReconcilePathsBackup finds the single reconcile-paths backup file in dir
// and decodes EVERY JSONL line, so a caller can assert the complete record set
// rather than only whichever record happens to land first.
func readReconcilePathsBackup(t *testing.T, dir string) []reconcilePathsBackupRecord {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "reconcile-paths-backup-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want exactly one backup file; got %v", matches)
	}
	b, err := os.ReadFile(matches[0]) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var recs []reconcilePathsBackupRecord
	for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec reconcilePathsBackupRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode backup line %d (%q): %v", i+1, line, err)
		}
		recs = append(recs, rec)
	}
	return recs
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

	recs := readReconcilePathsBackup(t, filepath.Dir(dbPath))
	if len(recs) != 1 {
		t.Fatalf("backup holds %d records, want exactly 1: %+v", len(recs), recs)
	}
	rec := recs[0]
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

// TestReconcilePaths_ScopedRelinkOnlyMatchesWithinLibrary: a --library-scoped
// run relinks a gone row to a present file in that same library, exercising the
// SweepOptions.LibraryID wiring on the relink path specifically (the delete
// path's library scoping is already covered by TestReconcilePaths_LibraryScoped).
//
// This is the POSITIVE half only. The complementary negative -- that a present
// file in a DIFFERENT library is never a relink target -- is covered by
// TestReconcilePaths_RelinkNeverCrossesLibraryBoundary below, because the two
// halves turn on different mechanisms: --library narrows which GONE rows are
// considered, while the present-file candidate pool is always scoped to the
// gone row's OWN library regardless of the flag.
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
	// The artifact: the work_queue row actually moved to the in-library path.
	if got := reconcilePathsWorkQueueSource(t, ctx, dbPath, oldPath, newPath); got != newPath {
		t.Errorf("work_queue.source_path = %q, want %q", got, newPath)
	}
}

// seedSecondLibrary adds a second library root (with one surviving file, so the
// availability guard treats it as mounted) and returns its path and id.
func seedSecondLibrary(t *testing.T, ctx context.Context, dbPath, dir, name string) (string, int64) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir second root: %v", err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open second library: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	lib, err := library.New(sqlDB).Add(ctx, dir, name, models.LibrarySettings{})
	if err != nil {
		t.Fatalf("library.Add second: %v", err)
	}
	return dir, lib.ID
}

// seedPresentFileInLibrary is seedReconcilePathsPresentFile for a library other
// than the default id 1.
func seedPresentFileInLibrary(t *testing.T, ctx context.Context, dbPath string, libraryID int64, filePath, mbid string) {
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
         VALUES (?, ?, 'Artist', 'Title', 'pending', ?, ?, ?)`,
		libraryID, filePath, filepath.Dir(filePath), filepath.Base(filePath), mbid,
	); err != nil {
		t.Fatalf("insert present scan_result in library %d: %v", libraryID, err)
	}
}

// reconcilePathsWorkQueueSource returns the source_path of the work_queue row
// currently at either wantOld or wantNew, so a caller can assert whether the
// relink moved it. Fails if neither is present.
func reconcilePathsWorkQueueSource(t *testing.T, ctx context.Context, dbPath, wantOld, wantNew string) string {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open work_queue: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	var got string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT source_path FROM work_queue WHERE source_path IN (?, ?) LIMIT 1`, wantOld, wantNew).Scan(&got); err != nil {
		t.Fatalf("query work_queue source_path (old=%q new=%q): %v", wantOld, wantNew, err)
	}
	return got
}

// TestReconcilePaths_RelinkNeverCrossesLibraryBoundary: the negative half of
// library scoping. A gone row in library A whose MBID matches a present file in
// library B is NOT relinked to it -- the present-file candidate pool is built
// per gone row from that row's OWN library_id, so a match in another library is
// invisible to it. Without that scoping, a reorganization that happened to
// duplicate an MBID across two libraries would silently re-point library A's
// row (and all its telemetry) at a file in library B.
//
// The run is deliberately UNSCOPED (no --library): the pool narrowing under
// test is not the flag's doing, so leaving the flag off proves the boundary
// holds on its own. Identity present with no match in its own library is a
// genuine delete under the CLI's PolicyFull, so the row is pruned, not relinked
// -- and the library-B file is untouched.
func TestReconcilePaths_RelinkNeverCrossesLibraryBoundary(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	otherRoot, otherLibID := seedSecondLibrary(t, ctx, dbPath, filepath.Join(filepath.Dir(root), "music2"), "lib2")

	// Gone row in library 1.
	oldPath := filepath.Join(root, "ArtistU", "AlbumOld", "01. track.flac")
	seedReconcilePathsRowWithIdentity(t, ctx, dbPath, oldPath, "mbid-u-shared")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old: %v", err)
	}
	// Keep library 1's root non-empty so it counts as available.
	seedReconcilePathsRow(t, ctx, dbPath, filepath.Join(root, "ArtistV", "01. kept.flac"))
	// The tempting-but-forbidden target: same MBID, different library.
	crossPath := filepath.Join(otherRoot, "ArtistU", "AlbumNew", "01. track.flac")
	seedPresentFileInLibrary(t, ctx, dbPath, otherLibID, crossPath, "mbid-u-shared")

	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if strings.Contains(buf.String(), "relinked 1 source") {
		t.Fatalf("relinked ACROSS a library boundary; got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "relinked 0 source") {
		t.Errorf("want 'relinked 0 source'; got: %s", buf.String())
	}
	// The artifact: no work_queue row points at the other library's file.
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	var crossed int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM work_queue WHERE source_path = ?`, crossPath).Scan(&crossed); err != nil {
		t.Fatalf("count crossed rows: %v", err)
	}
	if crossed != 0 {
		t.Errorf("%d work_queue row(s) were re-pointed at %s in another library, want 0", crossed, crossPath)
	}
	// Library B's own scan_results row is untouched by library A's reconcile.
	var otherSurvives int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM scan_results WHERE file_path = ?`, crossPath).Scan(&otherSurvives); err != nil {
		t.Fatalf("count other-library row: %v", err)
	}
	if otherSurvives != 1 {
		t.Errorf("the other library's scan_results row was disturbed: %d rows, want 1", otherSurvives)
	}
}
