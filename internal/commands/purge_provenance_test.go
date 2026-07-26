package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/purgeprovenance"
	"github.com/sydlexius/canticle/internal/queue"
)

// writePurgeSidecar writes a minimal .lrc sidecar, optionally carrying a
// [source:] header tag.
func writePurgeSidecar(t *testing.T, path, source string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "[00:01.00]hello\n"
	if source != "" {
		body = "[source:" + source + "]\n" + body
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

// seedPurgeTrack inserts a scan_results row (status 'done') and a linked
// work_queue item whose output is (outdir, filename), forcing its status to
// wqStatus.
func seedPurgeTrack(t *testing.T, ctx context.Context, dbPath, outdir, filename, wqStatus string) {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, outdir, filename, status) VALUES (1, ?, 'Artist', 'Title', ?, ?, 'done')`,
		filepath.Join(outdir, filename), outdir, filename)
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
		Track:        models.Track{ArtistName: "Artist", TrackName: filename},
		Outdir:       outdir,
		Filename:     filename,
		SourcePath:   filepath.Join(outdir, filename+".flac"),
		ScanResultID: srID,
		OutputPaths:  []models.OutputPath{{Outdir: outdir, Filename: filename}},
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = ? WHERE id = ?`, wqStatus, item.ID); err != nil {
		t.Fatalf("set wq status: %v", err)
	}
}

func purgeProvenanceCfg(t *testing.T, cfgPath, dbPath string) {
	t.Helper()
	content := "[db]\npath = \"" + strings.ReplaceAll(dbPath, `\`, `\\`) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func setupPurgeProvenance(t *testing.T) (ctx context.Context, cfgPath, dbPath, root string) {
	t.Helper()
	ctx = context.Background()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	cfgPath = filepath.Join(dir, "config.toml")
	root = filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	purgeProvenanceCfg(t, cfgPath, dbPath)
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	_ = sqlDB.Close()
	return ctx, cfgPath, dbPath, root
}

// TestPurgeProvenance_RequiresExactlyOneFilter: neither or both of
// --source/--no-source is rejected before touching the database.
func TestPurgeProvenance_RequiresExactlyOneFilter(t *testing.T) {
	ctx, cfgPath, _, _ := setupPurgeProvenance(t)

	var buf bytes.Buffer
	if code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath}); code != 2 {
		t.Fatalf("neither flag: exit=%d, want 2; out=%s", code, buf.String())
	}

	buf.Reset()
	if code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", NoSource: true}); code != 2 {
		t.Fatalf("both flags: exit=%d, want 2; out=%s", code, buf.String())
	}
}

// TestPurgeProvenance_DryRunWritesNothing: without --yes the command reports
// what would be deleted but deletes no file, mutates no row, and writes no
// backup.
func TestPurgeProvenance_DryRunWritesNothing(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	target := filepath.Join(root, "ArtistA", "one.lrc")
	writePurgeSidecar(t, target, "musixmatch")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(target), "one.lrc", "done")

	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch"})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "would delete 1") {
		t.Errorf("want 'would delete 1'; got: %s", buf.String())
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Errorf("dry-run must not delete: %v", statErr)
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "purge-provenance-backup-*.jsonl")); len(matches) != 0 {
		t.Errorf("dry-run wrote a backup: %v", matches)
	}
}

// TestPurgeProvenance_ApplyDeletesBacksUpAndRequeues: --yes deletes the
// matched sidecar, requeues its coupled rows, and writes a decodable JSONL
// backup written before the delete.
func TestPurgeProvenance_ApplyDeletesBacksUpAndRequeues(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	target := filepath.Join(root, "ArtistA", "one.lrc")
	other := filepath.Join(root, "ArtistB", "two.lrc")
	writePurgeSidecar(t, target, "musixmatch")
	writePurgeSidecar(t, other, "petitlyrics")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(target), "one.lrc", "done")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(other), "two.lrc", "done")

	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "deleted 1") {
		t.Errorf("want 'deleted 1'; got: %s", buf.String())
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Errorf("apply must delete the matched sidecar")
	}
	if _, statErr := os.Stat(other); statErr != nil {
		t.Errorf("apply must leave the non-matching sidecar: %v", statErr)
	}

	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "purge-provenance-backup-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want exactly one backup file; got %v", matches)
	}
	b, err := os.ReadFile(matches[0]) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var rec purgeProvenanceBackupRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("decode backup: %v", err)
	}
	if rec.Path != target || len(rec.ScanResultIDs) != 1 || len(rec.WorkItemIDs) != 1 {
		t.Errorf("backup record = %+v; want path=%q with 1 scan/1 wq id", rec, target)
	}

	// Second run finds nothing left to purge for this source.
	var buf2 bytes.Buffer
	if code := runPurgeProvenance(ctx, &buf2, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true}); code != 0 {
		t.Fatalf("second run exit=%d out=%s", code, buf2.String())
	}
	if !strings.Contains(buf2.String(), "deleted 0") {
		t.Errorf("second run want 'deleted 0'; got: %s", buf2.String())
	}
}

// TestPurgeProvenance_BackupRoundTripsFileContent proves the JSONL record is
// genuinely restorable: after a purge deletes the sidecar AND invalidates the
// lyrics_cache row that held the same text, the record is the only surviving
// copy. Reconstructing the file from it must reproduce the original bytes
// EXACTLY -- byte-for-byte, including the header, CRLF line endings and the
// absence of a trailing newline.
func TestPurgeProvenance_BackupRoundTripsFileContent(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	target := filepath.Join(root, "ArtistA", "one.lrc")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Deliberately awkward bytes: CRLF, a UTF-8 BOM, non-ASCII text, and no
	// trailing newline. A record that normalizes any of these is not a backup.
	original := []byte("\xef\xbb\xbf[source:musixmatch]\r\n[ar:Ärtist]\r\n[00:01.00]línea uno\r\n[00:02.00]last, no newline")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(target), "one.lrc", "done")

	// Precondition: the file exists with exactly these bytes before the purge,
	// so a later equality check cannot pass on an unchanged no-op.
	if pre, err := os.ReadFile(target); err != nil || !bytes.Equal(pre, original) {
		t.Fatalf("precondition: sidecar not written as expected (err=%v)", err)
	}

	var buf bytes.Buffer
	if code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("precondition: the sidecar must actually be deleted (err=%v)", err)
	}

	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "purge-provenance-backup-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want exactly one backup file; got %v", matches)
	}
	b, err := os.ReadFile(matches[0]) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var rec purgeProvenanceBackupRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("decode backup: %v", err)
	}
	if rec.Path != target {
		t.Fatalf("backup path = %q, want %q", rec.Path, target)
	}

	// Restore the file from the record alone and compare to the original.
	restored := filepath.Join(t.TempDir(), "restored.lrc")
	if err := os.WriteFile(restored, rec.Content, 0o600); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(restored) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("restored bytes differ from the original:\n got  %q\n want %q", got, original)
	}
}

// TestAppendPurgeProvenanceBackup_ContentCaptureFailureIsAnError: the content
// read happens inside the backup-first window, so a capture failure must
// surface as an error. purgeprovenance.processSidecar treats a Report error as
// "leave this sidecar untouched", so this error IS the guarantee that a file is
// never deleted without a restorable copy of its bytes.
func TestAppendPurgeProvenanceBackup_ContentCaptureFailureIsAnError(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "backup.jsonl")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	// Vanished sidecar: the record cannot be completed.
	if err := appendPurgeProvenanceBackup(f, purgeprovenance.Record{Path: filepath.Join(dir, "gone.lrc")}); err == nil {
		t.Error("a sidecar whose bytes cannot be read must fail the backup, not be silently recorded empty")
	}

	// Symlinked sidecar: never followed, here as elsewhere.
	realFile := filepath.Join(dir, "real.lrc")
	if err := os.WriteFile(realFile, []byte("[source:x]\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "link.lrc")
	if err := os.Symlink(realFile, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := appendPurgeProvenanceBackup(f, purgeprovenance.Record{Path: link}); err == nil {
		t.Error("a symlinked sidecar must fail the backup rather than be followed")
	}

	// The backup file must still be empty: no partial record was written.
	b, err := os.ReadFile(filepath.Join(dir, "backup.jsonl")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("a failed capture wrote a record anyway: %q", b)
	}
}

// TestPurgeProvenance_WalkErrorStillPrintsSummaryAndPurges: an unreadable
// subdirectory is a per-entry error, so the run continues past it, the summary
// is still printed (the operator's only record of what was destroyed), and the
// exit code is non-zero because errors were counted.
func TestPurgeProvenance_WalkErrorStillPrintsSummaryAndPurges(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode-0000 directory is still readable")
	}
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	blocked := filepath.Join(root, "A-blocked")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "B-after", "after.lrc")
	writePurgeSidecar(t, target, "musixmatch")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(target), "after.lrc", "done")

	if _, err := os.Stat(target); err != nil {
		t.Fatalf("precondition: the sidecar must exist before the run: %v", err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true})
	if code == 0 {
		t.Errorf("counted errors must yield a non-zero exit; got 0. out=%s", buf.String())
	}
	if !strings.Contains(buf.String(), "purge-provenance: scanned") {
		t.Errorf("the summary must print even when the walk hit errors; got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "deleted 1") {
		t.Errorf("the run must continue past the unreadable directory; got: %s", buf.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the sidecar past the unreadable directory was not purged: %v", err)
	}
}

// TestPurgeProvenance_SourceIsTrimmed: the guard trimmed --source but the
// filter used the raw value, so `--source " musixmatch"` passed validation and
// then matched nothing. A padded value must select exactly the same set as the
// unpadded one.
func TestPurgeProvenance_SourceIsTrimmed(t *testing.T) {
	run := func(t *testing.T, src string) string {
		t.Helper()
		ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
		target := filepath.Join(root, "ArtistA", "one.lrc")
		other := filepath.Join(root, "ArtistB", "two.lrc")
		writePurgeSidecar(t, target, "musixmatch")
		writePurgeSidecar(t, other, "petitlyrics")
		seedPurgeTrack(t, ctx, dbPath, filepath.Dir(target), "one.lrc", "done")
		seedPurgeTrack(t, ctx, dbPath, filepath.Dir(other), "two.lrc", "done")
		var buf bytes.Buffer
		if code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: src}); code != 0 {
			t.Fatalf("exit=%d out=%s", code, buf.String())
		}
		return buf.String()
	}

	exact := run(t, "musixmatch")
	// Precondition: the unpadded filter really does match something, so an
	// equality check below cannot pass on two empty sets.
	if !strings.Contains(exact, "would delete: ") || !strings.Contains(exact, "one.lrc") {
		t.Fatalf("precondition: --source musixmatch must match one.lrc; got: %s", exact)
	}
	padded := run(t, "  musixmatch  ")
	if !strings.Contains(padded, "one.lrc") {
		t.Errorf("a padded --source matched nothing; got: %s", padded)
	}
	if strings.Contains(padded, "two.lrc") {
		t.Errorf("a padded --source over-matched; got: %s", padded)
	}
}

// TestPurgeProvenance_DryRunCountMatchesApply: the preview must promise exactly
// what apply performs. res.Matched is incremented before the in-flight check,
// so a sidecar whose linked work item is 'processing' inflated the dry-run
// "would delete" count while apply mode correctly skipped it.
func TestPurgeProvenance_DryRunCountMatchesApply(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	free := filepath.Join(root, "ArtistA", "free.lrc")
	inflight := filepath.Join(root, "ArtistB", "inflight.lrc")
	writePurgeSidecar(t, free, "musixmatch")
	writePurgeSidecar(t, inflight, "musixmatch")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(free), "free.lrc", "done")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(inflight), "inflight.lrc", "processing")

	var dry bytes.Buffer
	if code := runPurgeProvenance(ctx, &dry, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch"}); code != 0 {
		t.Fatalf("dry-run exit=%d out=%s", code, dry.String())
	}
	// Precondition: the fixture really does contain an in-flight match, else
	// the two counts agree trivially.
	if !strings.Contains(dry.String(), "1 skipped in-flight") {
		t.Fatalf("precondition: fixture must produce exactly one in-flight skip; got: %s", dry.String())
	}
	if !strings.Contains(dry.String(), "would delete 1,") {
		t.Errorf("dry run over-counts: it must not promise to delete the in-flight sidecar; got: %s", dry.String())
	}

	var apply bytes.Buffer
	if code := runPurgeProvenance(ctx, &apply, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, apply.String())
	}
	if !strings.Contains(apply.String(), "deleted 1,") {
		t.Errorf("apply deleted a different count than the preview promised; got: %s", apply.String())
	}
	// Artifact check: only the non-in-flight sidecar is gone.
	if _, err := os.Stat(free); !os.IsNotExist(err) {
		t.Errorf("the free sidecar should have been deleted: %v", err)
	}
	if _, err := os.Stat(inflight); err != nil {
		t.Errorf("the in-flight sidecar must survive: %v", err)
	}
}

// TestPurgeProvenance_NoSourceCohort: --no-source targets sidecars with no
// [source:] tag.
func TestPurgeProvenance_NoSourceCohort(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	foreign := filepath.Join(root, "ArtistA", "foreign.lrc")
	tagged := filepath.Join(root, "ArtistA", "tagged.lrc")
	writePurgeSidecar(t, foreign, "")
	writePurgeSidecar(t, tagged, "musixmatch")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(foreign), "foreign.lrc", "done")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(tagged), "tagged.lrc", "done")

	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, NoSource: true, Yes: true})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if _, statErr := os.Stat(foreign); !os.IsNotExist(statErr) {
		t.Errorf("no-source cohort must be deleted")
	}
	if _, statErr := os.Stat(tagged); statErr != nil {
		t.Errorf("tagged sidecar must survive: %v", statErr)
	}
}

// TestPurgeProvenance_ProcessingRowsSkipped: a matched sidecar whose linked
// work_queue row is 'processing' is left untouched end-to-end.
func TestPurgeProvenance_ProcessingRowsSkipped(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	target := filepath.Join(root, "ArtistA", "one.lrc")
	writePurgeSidecar(t, target, "musixmatch")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(target), "one.lrc", "processing")

	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "1 skipped in-flight") {
		t.Errorf("want '1 skipped in-flight'; got: %s", buf.String())
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Errorf("processing row's sidecar must survive: %v", statErr)
	}
}

// TestPurgeProvenance_SymlinksSkipped: a symlinked sidecar is never deleted or
// counted as matched, even when it points at content that would match.
func TestPurgeProvenance_SymlinksSkipped(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	real := filepath.Join(root, "ArtistA", "real.lrc")
	link := filepath.Join(root, "ArtistA", "link.lrc")
	writePurgeSidecar(t, real, "musixmatch")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(real), "real.lrc", "done")

	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "1 skipped symlink") {
		t.Errorf("want '1 skipped symlink'; got: %s", buf.String())
	}
	if _, statErr := os.Lstat(link); statErr != nil {
		t.Errorf("symlink itself must survive: %v", statErr)
	}
}

// TestPurgeProvenance_EnumerationConfinedToRoots: a matching sidecar outside
// the configured library root is never examined or touched.
func TestPurgeProvenance_EnumerationConfinedToRoots(t *testing.T) {
	ctx, cfgPath, _, _ := setupPurgeProvenance(t)
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.lrc")
	writePurgeSidecar(t, outside, "musixmatch")

	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "scanned 0 sidecar") {
		t.Errorf("want 'scanned 0 sidecar'; got: %s", buf.String())
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Errorf("out-of-root sidecar must survive: %v", statErr)
	}
}

// TestPurgeProvenance_SummaryCounts: the printed summary line reflects the
// actual deleted/requeued totals for a mixed batch.
func TestPurgeProvenance_SummaryCounts(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	a := filepath.Join(root, "ArtistA", "a.lrc")
	b := filepath.Join(root, "ArtistA", "b.lrc")
	c := filepath.Join(root, "ArtistA", "c.lrc")
	writePurgeSidecar(t, a, "musixmatch")
	writePurgeSidecar(t, b, "musixmatch")
	writePurgeSidecar(t, c, "petitlyrics")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(a), "a.lrc", "done")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(b), "b.lrc", "done")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(c), "c.lrc", "done")

	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "scanned 3 sidecar(s); deleted 2, requeued 2 (2 scan_results reset") {
		t.Errorf("unexpected summary line: %s", buf.String())
	}
}

// TestPurgeProvenance_LibraryScoped: --library narrows enumeration to the
// named library's root only.
func TestPurgeProvenance_LibraryScoped(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	target := filepath.Join(root, "ArtistA", "one.lrc")
	writePurgeSidecar(t, target, "musixmatch")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(target), "one.lrc", "done")

	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Library: "lib", Yes: true})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "deleted 1") {
		t.Errorf("want 'deleted 1'; got: %s", buf.String())
	}
}

// TestPurgeProvenance_LibraryNotFound: an unknown --library exits 1.
func TestPurgeProvenance_LibraryNotFound(t *testing.T) {
	ctx, cfgPath, _, _ := setupPurgeProvenance(t)
	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Library: "no-such-library"})
	if code != 1 {
		t.Fatalf("exit=%d want 1; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "not found") {
		t.Errorf("want 'not found'; got: %s", buf.String())
	}
}

// TestPurgeProvenance_NoLibrariesConfigured: with zero libraries in the
// database, the command exits 0 with an explanatory message rather than
// erroring or silently doing nothing.
func TestPurgeProvenance_NoLibrariesConfigured(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.toml")
	purgeProvenanceCfg(t, cfgPath, dbPath)
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	_ = sqlDB.Close()

	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch"})
	if code != 0 {
		t.Fatalf("exit=%d want 0; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "no library roots configured") {
		t.Errorf("want 'no library roots configured'; got: %s", buf.String())
	}
}

// TestPurgeProvenance_BackupOpenFailureCountsErrorAndExitsNonZero: when the
// backup file cannot be opened (an invalid --backup directory), the matched
// sidecar's delete is skipped (backup-first), the error is counted, and the
// process exits non-zero so a script driving this destructive command can
// detect the partial run rather than reading "success".
func TestPurgeProvenance_BackupOpenFailureCountsErrorAndExitsNonZero(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	target := filepath.Join(root, "ArtistA", "one.lrc")
	writePurgeSidecar(t, target, "musixmatch")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(target), "one.lrc", "done")

	// A --backup path under a nonexistent directory: os.OpenFile fails.
	badBackup := filepath.Join(root, "no-such-dir", "backup.jsonl")

	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true, Backup: badBackup})
	if code != 1 {
		t.Fatalf("exit=%d want 1; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "1 errors") {
		t.Errorf("want '1 errors' in summary; got: %s", buf.String())
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Errorf("a backup-open failure must leave the sidecar in place: %v", statErr)
	}
}

// TestRunScanCmd_DispatchesPurgeProvenance exercises runScanCmd's actual
// dispatch switch (not just the handler directly), covering both the direct
// sub-config path and the parent --config inheritance path -- the same
// pattern every other reconcile-family command uses to cover its dispatch
// case.
func TestRunScanCmd_DispatchesPurgeProvenance(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	target := filepath.Join(root, "ArtistA", "one.lrc")
	writePurgeSidecar(t, target, "musixmatch")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(target), "one.lrc", "done")

	// Direct sub-config.
	var buf bytes.Buffer
	if rc := runScanCmd(ctx, &buf, ScanCmd{PurgeProvenance: &ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch"}}); rc != 0 {
		t.Fatalf("dispatch rc=%d out=%s", rc, buf.String())
	}
	if !strings.Contains(buf.String(), "would delete 1") {
		t.Errorf("dispatch output: %q", buf.String())
	}

	// Parent --config inherited when the subcommand omits it.
	var buf2 bytes.Buffer
	if rc := runScanCmd(ctx, &buf2, ScanCmd{ConfigPath: cfgPath, PurgeProvenance: &ScanPurgeProvenanceCmd{Source: "musixmatch"}}); rc != 0 {
		t.Fatalf("inherited-config rc=%d out=%s", rc, buf2.String())
	}
	if !strings.Contains(buf2.String(), "would delete 1") {
		t.Errorf("inherited-config output: %q", buf2.String())
	}
}

// TestPurgeProvenance_IsRecognizedSubcommand guards the dispatch wiring so
// "scan purge-provenance" routes to the new handler.
func TestPurgeProvenance_IsRecognizedSubcommand(t *testing.T) {
	if !usesSubcommand([]string{"scan", "purge-provenance"}) {
		t.Error("`scan purge-provenance` not recognized as a subcommand invocation")
	}
}

// TestPurgeProvenance_ConfigLoadError: an unreadable/invalid config exits 1.
func TestPurgeProvenance_ConfigLoadError(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(cfgPath, []byte("this is not = valid = toml ]["), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	var buf bytes.Buffer
	code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch"})
	if code != 1 {
		t.Fatalf("exit=%d want 1 for invalid config", code)
	}
}

// TestPurgeProvenance_SummaryReportsCacheInvalidation: the operator-facing
// summary must say how many cache entries were invalidated. Without it the
// summary's "requeued N" reads as a re-fetch guarantee the command could not
// have made before the invalidation existed.
func TestPurgeProvenance_SummaryReportsCacheInvalidation(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	target := filepath.Join(root, "ArtistA", "one.lrc")
	writePurgeSidecar(t, target, "musixmatch")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(target), "one.lrc", "done")

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := cache.New(sqlDB).Store(ctx, "Artist", "Title", 0, "[00:01.00]cached\n"); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	var buf bytes.Buffer
	if code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "1 cache entries invalidated") {
		t.Errorf("summary must report the cache invalidation; got: %s", buf.String())
	}
}

// TestPurgeProvenance_SummaryNotesUnlinkedSidecars: a deleted sidecar with no
// linked scan_results row gets no requeue and no cache invalidation, so the
// summary must say so rather than imply the re-fetch guarantee covers it.
func TestPurgeProvenance_SummaryNotesUnlinkedSidecars(t *testing.T) {
	ctx, cfgPath, _, root := setupPurgeProvenance(t)
	writePurgeSidecar(t, filepath.Join(root, "ArtistA", "orphan.lrc"), "musixmatch")

	var buf bytes.Buffer
	if code := runPurgeProvenance(ctx, &buf, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "no linked scan_results row") {
		t.Errorf("summary must flag the unlinked sidecar; got: %s", buf.String())
	}
}
