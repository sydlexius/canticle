package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/doxazo-net/canticle/internal/db"
	"github.com/doxazo-net/canticle/internal/lyrics"
	"github.com/doxazo-net/canticle/internal/models"
	"github.com/doxazo-net/canticle/internal/queue"
)

func writeMarkerProvCfg(t *testing.T, path, dbPath string) {
	t.Helper()
	content := "[db]\npath = \"" + strings.ReplaceAll(dbPath, `\`, `\\`) + "\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeMarkerProvCfg: %v", err)
	}
}

// seedDetectorMarker enqueues a row, marks it detector-instrumental (result=1,
// done, dv set), and writes a BARE marker .txt at its output location.
func seedDetectorMarker(t *testing.T, ctx context.Context, dbPath, outdir, detectorVersion string) {
	t.Helper()
	seedDetectorMarkerRow(t, ctx, dbPath, outdir, "song", detectorVersion, true)
}

// seedDetectorMarkerRow is the general form of seedDetectorMarker: it enqueues
// a row under a caller-chosen basename, marks it detector-instrumental, and
// optionally writes a bare marker .txt at its output location (writeMarker=false
// lets a test cover the marker-absent-on-disk skip path).
func seedDetectorMarkerRow(t *testing.T, ctx context.Context, dbPath, outdir, basename, detectorVersion string, writeMarker bool) {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	q := queue.NewDBQueue(sqlDB)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:       models.Track{ArtistName: "Artist", TrackName: basename},
		Outdir:      outdir,
		Filename:    basename + ".lrc",
		SourcePath:  filepath.Join(outdir, basename+".flac"),
		OutputPaths: []models.OutputPath{{Outdir: outdir, Filename: basename + ".lrc"}},
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, item.ID, 0, nil); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET instrumental_result=1, status='done', outcome_type='instrumental', detector_version=? WHERE id=?`,
		detectorVersion, item.ID); err != nil {
		t.Fatalf("mark detector-instrumental: %v", err)
	}
	if !writeMarker {
		return
	}
	// Bare marker on disk (old writeInstrumental format).
	if err := os.WriteFile(filepath.Join(outdir, basename+".txt"), []byte(lyrics.InstrumentalMarker+"\n"), 0o644); err != nil {
		t.Fatalf("write bare marker: %v", err)
	}
}

func TestRunReconcileMarkerProvenance_DryRunThenApply(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeMarkerProvCfg(t, cfgPath, dbPath)
	seedDetectorMarker(t, ctx, dbPath, outdir, "1.5.0")
	markerPath := filepath.Join(outdir, "song.txt")

	// Dry run: previews, changes nothing.
	var dry bytes.Buffer
	if code := runReconcileMarkerProvenance(ctx, &dry, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("dry-run exit=%d out=%s", code, dry.String())
	}
	if !strings.Contains(dry.String(), "would stamp 1") {
		t.Errorf("dry-run summary missing 'would stamp 1':\n%s", dry.String())
	}
	prov, _, _ := lyrics.ReadInstrumentalProvenance(markerPath)
	if prov.Source != "" {
		t.Fatalf("dry-run must not modify the marker; got source %q", prov.Source)
	}

	// Apply.
	var apply bytes.Buffer
	if code := runReconcileMarkerProvenance(ctx, &apply, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, apply.String())
	}
	if !strings.Contains(apply.String(), "stamped 1") {
		t.Errorf("apply summary missing 'stamped 1':\n%s", apply.String())
	}
	prov, isMarker, err := lyrics.ReadInstrumentalProvenance(markerPath)
	if err != nil || !isMarker {
		t.Fatalf("re-read after apply: isMarker=%v err=%v", isMarker, err)
	}
	if prov.Source != lyrics.SourceDetector || prov.DetectorVersion != "1.5.0" {
		t.Fatalf("marker not stamped: %+v", prov)
	}

	// Idempotent: a second apply stamps nothing.
	var again bytes.Buffer
	if code := runReconcileMarkerProvenance(ctx, &again, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("second apply exit=%d", code)
	}
	if !strings.Contains(again.String(), "stamped 0") {
		t.Errorf("second apply should stamp 0 (already headed):\n%s", again.String())
	}
}

// TestRunReconcileMarkerProvenance_MarkerAbsent verifies a detector row whose
// .txt marker is missing from disk is skipped cleanly (no crash, no false
// stamp), and the summary reports 0 stamped.
func TestRunReconcileMarkerProvenance_MarkerAbsent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeMarkerProvCfg(t, cfgPath, dbPath)
	seedDetectorMarkerRow(t, ctx, dbPath, outdir, "absent", "1.5.0", false)

	var out bytes.Buffer
	if code := runReconcileMarkerProvenance(ctx, &out, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "would stamp 0") {
		t.Errorf("summary missing 'would stamp 0' for an absent marker:\n%s", out.String())
	}

	// Apply too: still nothing to stamp, still exits clean.
	var applyOut bytes.Buffer
	if code := runReconcileMarkerProvenance(ctx, &applyOut, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, applyOut.String())
	}
	if !strings.Contains(applyOut.String(), "stamped 0") {
		t.Errorf("apply summary missing 'stamped 0' for an absent marker:\n%s", applyOut.String())
	}
}

// TestRunReconcileMarkerProvenance_MixedBatchStampsOnlyBare seeds one bare
// marker and one already-headed marker, applies, and asserts only the bare
// one gets stamped while the headed one is left byte-for-byte untouched.
func TestRunReconcileMarkerProvenance_MixedBatchStampsOnlyBare(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeMarkerProvCfg(t, cfgPath, dbPath)

	seedDetectorMarkerRow(t, ctx, dbPath, outdir, "bare", "1.5.0", true)
	seedDetectorMarkerRow(t, ctx, dbPath, outdir, "headed", "1.5.0", false)
	headedPath := filepath.Join(outdir, "headed.txt")
	headedOrig := "[by:canticle]\n[source:canticle-detector]\n[dv:9.9.9]\n" + lyrics.InstrumentalMarker + "\n"
	if err := os.WriteFile(headedPath, []byte(headedOrig), 0o644); err != nil {
		t.Fatalf("write headed marker: %v", err)
	}

	var out bytes.Buffer
	if code := runReconcileMarkerProvenance(ctx, &out, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "stamped 1") {
		t.Errorf("summary missing 'stamped 1' for a mixed batch:\n%s", out.String())
	}

	barePath := filepath.Join(outdir, "bare.txt")
	prov, isMarker, err := lyrics.ReadInstrumentalProvenance(barePath)
	if err != nil || !isMarker || prov.Source != lyrics.SourceDetector {
		t.Fatalf("bare marker not stamped: isMarker=%v prov=%+v err=%v", isMarker, prov, err)
	}

	headedData, err := os.ReadFile(headedPath)
	if err != nil {
		t.Fatalf("read headed marker: %v", err)
	}
	if string(headedData) != headedOrig {
		t.Fatalf("already-headed marker must be untouched:\n%s", headedData)
	}
}

// TestRunReconcileMarkerProvenance_EmptyDetectorVersionOmitsDVTag verifies a
// detector row with an empty stored detector_version gets [source:] stamped
// but no [dv:] header.
func TestRunReconcileMarkerProvenance_EmptyDetectorVersionOmitsDVTag(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeMarkerProvCfg(t, cfgPath, dbPath)
	seedDetectorMarkerRow(t, ctx, dbPath, outdir, "nodv", "", true)

	var out bytes.Buffer
	if code := runReconcileMarkerProvenance(ctx, &out, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "stamped 1") {
		t.Errorf("summary missing 'stamped 1':\n%s", out.String())
	}

	markerPath := filepath.Join(outdir, "nodv.txt")
	prov, isMarker, err := lyrics.ReadInstrumentalProvenance(markerPath)
	if err != nil || !isMarker {
		t.Fatalf("re-read: isMarker=%v err=%v", isMarker, err)
	}
	if prov.Source != lyrics.SourceDetector {
		t.Fatalf("Source = %q, want %q", prov.Source, lyrics.SourceDetector)
	}
	if prov.DetectorVersion != "" {
		t.Fatalf("DetectorVersion = %q, want empty", prov.DetectorVersion)
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if strings.Contains(string(data), "[dv:") {
		t.Fatalf("empty detector_version must not emit a [dv:] header:\n%s", data)
	}
}

// TestRunReconcileMarkerProvenance_LimitCapsRowsConsidered verifies --limit
// caps how many detector rows are even considered, not just how many markers
// end up stamped.
func TestRunReconcileMarkerProvenance_LimitCapsRowsConsidered(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeMarkerProvCfg(t, cfgPath, dbPath)
	seedDetectorMarkerRow(t, ctx, dbPath, outdir, "one", "1.0.0", true)
	seedDetectorMarkerRow(t, ctx, dbPath, outdir, "two", "1.0.0", true)

	var out bytes.Buffer
	if code := runReconcileMarkerProvenance(ctx, &out, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath, Limit: 1}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "scanned 1 detector marker row") {
		t.Errorf("summary must report exactly 1 row scanned under --limit 1:\n%s", out.String())
	}
}

// TestRunReconcileMarkerProvenance_BackupFileContainsStampedPath verifies the
// JSONL backup file is created on apply and records the stamped file's path.
func TestRunReconcileMarkerProvenance_BackupFileContainsStampedPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeMarkerProvCfg(t, cfgPath, dbPath)
	seedDetectorMarkerRow(t, ctx, dbPath, outdir, "backup", "2.0.0", true)
	backupPath := filepath.Join(dir, "backup.jsonl")

	var out bytes.Buffer
	if code := runReconcileMarkerProvenance(ctx, &out, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath, Yes: true, Backup: backupPath}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "backup written to "+backupPath) {
		t.Errorf("summary missing backup-path line:\n%s", out.String())
	}

	data, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup file: %v", err)
	}
	markerPath := filepath.Join(outdir, "backup.txt")
	if !strings.Contains(string(data), markerPath) {
		t.Fatalf("backup file missing stamped path %q:\n%s", markerPath, data)
	}
	if !strings.Contains(string(data), lyrics.SourceDetector) {
		t.Fatalf("backup file missing source token:\n%s", data)
	}
}

// TestRunScanCmd_DispatchesReconcileMarkerProvenance verifies the scan
// subcommand switch routes to runReconcileMarkerProvenance, both with an
// explicit sub-config and with the parent --config inherited (mirrors
// the reconcile-lrc dispatch test).
func TestRunScanCmd_DispatchesReconcileMarkerProvenance(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeMarkerProvCfg(t, cfgPath, dbPath)
	seedDetectorMarker(t, ctx, dbPath, outdir, "1.5.0")

	// Direct sub-config.
	var buf bytes.Buffer
	if rc := runScanCmd(ctx, &buf, ScanCmd{ReconcileMarkerProvenance: &ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath}}); rc != 0 {
		t.Fatalf("dispatch rc=%d out=%s", rc, buf.String())
	}
	if !strings.Contains(buf.String(), "would stamp 1") {
		t.Errorf("dispatch output: %q", buf.String())
	}

	// Parent --config inherited when the subcommand omits it.
	var buf2 bytes.Buffer
	if rc := runScanCmd(ctx, &buf2, ScanCmd{ConfigPath: cfgPath, ReconcileMarkerProvenance: &ScanReconcileMarkerProvenanceCmd{}}); rc != 0 {
		t.Fatalf("inherited-config rc=%d out=%s", rc, buf2.String())
	}
}

// TestRunReconcileMarkerProvenance_UnknownLibraryFailsClean verifies an
// unresolvable --library value fails the shared env setup and returns a
// non-zero exit rather than proceeding against every library.
func TestRunReconcileMarkerProvenance_UnknownLibraryFailsClean(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.toml")
	writeMarkerProvCfg(t, cfgPath, dbPath)
	// Touch the DB so it exists with the schema before the command opens it.
	seedDetectorMarkerRow(t, ctx, dbPath, t.TempDir(), "unused", "1.0.0", false)

	var out bytes.Buffer
	code := runReconcileMarkerProvenance(ctx, &out, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath, Library: "no-such-library"})
	if code == 0 {
		t.Fatalf("expected non-zero exit for an unresolvable library, got 0: %s", out.String())
	}
	if !strings.Contains(out.String(), "library") {
		t.Errorf("expected an operator-facing library-not-found message, got: %q", out.String())
	}
}

// TestRunReconcileMarkerProvenance_SymlinkedMarkerIsSkippedNotStamped
// verifies a detector row whose marker path resolves through a symlink is
// read successfully (isMarker=true through the symlink) but is left
// unstamped: WriteMarkerProvenance intentionally refuses to rewrite through a
// symlink, so the command must count it as skipped, not stamped or errored.
func TestRunReconcileMarkerProvenance_SymlinkedMarkerIsSkippedNotStamped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeMarkerProvCfg(t, cfgPath, dbPath)
	seedDetectorMarkerRow(t, ctx, dbPath, outdir, "linked", "1.5.0", false)

	real := filepath.Join(dir, "real-marker.txt")
	if err := os.WriteFile(real, []byte(lyrics.InstrumentalMarker+"\n"), 0o644); err != nil {
		t.Fatalf("write real marker: %v", err)
	}
	linkPath := filepath.Join(outdir, "linked.txt")
	if err := os.Symlink(real, linkPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	var out bytes.Buffer
	if code := runReconcileMarkerProvenance(ctx, &out, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "stamped 0") || !strings.Contains(out.String(), "skipped 1") {
		t.Errorf("expected a symlinked marker to be skipped, not stamped:\n%s", out.String())
	}
	// The symlink target must be untouched (still bare, no header).
	prov, isMarker, err := lyrics.ReadInstrumentalProvenance(real)
	if err != nil || !isMarker {
		t.Fatalf("re-read real marker: isMarker=%v err=%v", isMarker, err)
	}
	if prov.Source != "" {
		t.Fatalf("symlinked marker must not be stamped: %+v", prov)
	}
}

// TestRunReconcileMarkerProvenance_MissingWorkQueueTableFailsClean covers a
// corrupted/mismatched database (the work_queue table gone, e.g. from a
// botched manual migration) failing the row listing cleanly instead of
// panicking.
func TestRunReconcileMarkerProvenance_MissingWorkQueueTableFailsClean(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.toml")
	writeMarkerProvCfg(t, cfgPath, dbPath)

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `DROP TABLE work_queue`); err != nil {
		t.Fatalf("drop work_queue: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var out bytes.Buffer
	code := runReconcileMarkerProvenance(ctx, &out, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath})
	if code != 1 {
		t.Fatalf("code = %d, want 1 for a missing work_queue table: %s", code, out.String())
	}
}

// TestRunReconcileMarkerProvenance_BackupPathIsDirectoryFailsClean covers an
// operator-supplied --backup path that collides with an existing directory:
// os.OpenFile(O_CREATE) on it fails, and the command must exit non-zero
// rather than silently drop the stamp record.
func TestRunReconcileMarkerProvenance_BackupPathIsDirectoryFailsClean(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeMarkerProvCfg(t, cfgPath, dbPath)
	seedDetectorMarker(t, ctx, dbPath, outdir, "1.5.0")

	backupDir := filepath.Join(dir, "backup-is-a-dir")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("mkdir backupDir: %v", err)
	}

	var out bytes.Buffer
	code := runReconcileMarkerProvenance(ctx, &out, ScanReconcileMarkerProvenanceCmd{ConfigPath: cfgPath, Yes: true, Backup: backupDir})
	if code != 1 {
		t.Fatalf("code = %d, want 1 when --backup collides with a directory: %s", code, out.String())
	}
}
