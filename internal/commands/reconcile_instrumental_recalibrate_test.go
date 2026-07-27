package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// writeRecalibrateCfg writes a minimal config with NO classifier configured.
// The re-DECISION still needs no sidecar -- it runs purely on telemetry already
// stamped on each row -- but the current MODEL version is then unknown (#684),
// and unknown never takes the destructive reset branch. Tests whose subject is
// the version comparison itself want writeRecalibrateCfgWithDetector.
func writeRecalibrateCfg(t *testing.T, path, dbPath string) {
	t.Helper()
	content := "[db]\npath = \"" + strings.ReplaceAll(dbPath, `\`, `\\`) + "\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeRecalibrateCfg: %v", err)
	}
}

// startModelVersionSidecar serves just enough of the classifier for the
// detector's model-version probe (#684) and returns its URL. Used by tests whose
// subject is the version COMPARISON: without a reachable sidecar the current
// version resolves to UNKNOWN, and unknown deliberately never takes the
// destructive reset branch, so such a test would be asserting the guard rather
// than the behavior it means to pin.
func startModelVersionSidecar(t *testing.T, modelVersion string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok","model_version":"` + modelVersion + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// writeRecalibrateCfgWithDetector is writeRecalibrateCfg plus a reachable
// classifier, so the current model version is KNOWABLE.
func writeRecalibrateCfgWithDetector(t *testing.T, path, dbPath, classifierURL string) {
	t.Helper()
	content := "[db]\npath = \"" + strings.ReplaceAll(dbPath, `\`, `\\`) + "\"\n" +
		"\n[instrumental_detector]\nenabled = true\nclassifier_url = \"" + classifierURL + "\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeRecalibrateCfgWithDetector: %v", err)
	}
}

// seedVocalGateRejection parks a deferred row already stamped
// instrumental_result=0 with re-decidable telemetry that PASSES the default
// thresholds (min_confidence=0.90, vocal_max=0.03, speech_max=0.20) -- the
// "old tight threshold buried it" case this command exists to reopen.
func seedVocalGateRejection(t *testing.T, ctx context.Context, dbPath, outdir, detectorVersion string) int64 {
	t.Helper()
	srcPath := filepath.Join(outdir, "song.flac")
	if err := os.WriteFile(srcPath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	q := queue.NewDBQueue(sqlDB)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:       models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:      outdir,
		Filename:    "song.lrc",
		SourcePath:  srcPath,
		OutputPaths: []models.OutputPath{{Outdir: outdir, Filename: "song.flac"}},
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
	tel := queue.InstrumentalTelemetry{
		MusicSum:        0.95,
		VocalPeak:       0.01,
		SpeechMean:      0.01,
		VocalClass:      "Singing",
		DetectorVersion: detectorVersion,
	}
	if err := q.SetInstrumentalResult(ctx, item.ID, 0, tel); err != nil {
		t.Fatalf("SetInstrumentalResult: %v", err)
	}
	return item.ID
}

// TestRunReconcileInstrumentalRecalibrate_NoClassifierRequired is the core
// requirement of #510: the command must work with NO classifier_url
// configured at all, because it re-decides from stored telemetry alone.
func TestRunReconcileInstrumentalRecalibrate_NoClassifierRequired(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRecalibrateCfg(t, cfgPath, dbPath)

	var buf bytes.Buffer
	code := runReconcileInstrumentalRecalibrate(ctx, &buf, ScanReconcileInstrumentalRecalibrateCmd{ConfigPath: cfgPath})
	if code != 0 {
		t.Fatalf("exit=%d out=%s (must not require a detector)", code, buf.String())
	}
	if !strings.Contains(buf.String(), "0 vocal-gate-rejected row(s) considered") {
		t.Errorf("unexpected output:\n%s", buf.String())
	}
}

// TestRunReconcileInstrumentalRecalibrate_DryRunSettlesNothing: a
// version-matched row that now passes the (default) thresholds must be
// previewed as "would settle" in a dry run, and the row/marker must stay
// untouched until --yes.
func TestRunReconcileInstrumentalRecalibrate_DryRunSettlesNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeRecalibrateCfg(t, cfgPath, dbPath)
	id := seedVocalGateRejection(t, ctx, dbPath, outdir, version)
	markerPath := filepath.Join(outdir, "song.txt")

	var dry bytes.Buffer
	if code := runReconcileInstrumentalRecalibrate(ctx, &dry, ScanReconcileInstrumentalRecalibrateCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("dry-run exit=%d out=%s", code, dry.String())
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write a marker; stat err=%v", err)
	}
	if !strings.Contains(dry.String(), "would settle") {
		t.Errorf("dry-run output missing 'would settle':\n%s", dry.String())
	}

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open verify: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	var status string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if status != "deferred" {
		t.Errorf("status = %q; want deferred (dry-run must not settle the row)", status)
	}
}

// TestRunReconcileInstrumentalRecalibrate_ApplySettlesVersionMatchedRow:
// under --yes, a version-matched passing row gets its marker written and is
// settled/completed.
func TestRunReconcileInstrumentalRecalibrate_ApplySettlesVersionMatchedRow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	// A reachable sidecar and a row seeded at the SAME model version, so this
	// settles because the versions MATCH -- which is what the test is named for.
	// With no classifier the version resolves to unknown and the row would settle
	// via the unknown-version guard instead: the test would then pass no matter
	// what version the row carried, asserting the guard rather than the match.
	const modelVersion = "current-model-sha"
	writeRecalibrateCfgWithDetector(t, cfgPath, dbPath, startModelVersionSidecar(t, modelVersion))
	id := seedVocalGateRejection(t, ctx, dbPath, outdir, modelVersion)
	markerPath := filepath.Join(outdir, "song.txt")

	var app bytes.Buffer
	if code := runReconcileInstrumentalRecalibrate(ctx, &app, ScanReconcileInstrumentalRecalibrateCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, app.String())
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("apply must write the instrumental marker: %v", err)
	}
	if !strings.Contains(app.String(), "settled=1") {
		t.Errorf("output should report the settle:\n%s", app.String())
	}
	if !strings.Contains(app.String(), "backup of changed rows written to") {
		t.Errorf("output should point at the backup file:\n%s", app.String())
	}

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open verify: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	var status string
	var result *int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, instrumental_result FROM work_queue WHERE id = ?`, id,
	).Scan(&status, &result); err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if status != "done" {
		t.Errorf("status = %q; want done", status)
	}
	if result == nil || *result != 1 {
		t.Errorf("instrumental_result = %v; want 1", result)
	}
}

// TestRunReconcileInstrumentalRecalibrate_VersionMismatchResets: a row scored
// by a different detector version must be reset to never-classified rather
// than settled on stale telemetry.
func TestRunReconcileInstrumentalRecalibrate_VersionMismatchResets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	// A reachable sidecar so the CURRENT model version is knowable: this test's
	// subject is a genuine version MISMATCH, and with the version unknown the
	// engine deliberately declines to reset (#684), which would make this test
	// pass for the wrong reason.
	writeRecalibrateCfgWithDetector(t, cfgPath, dbPath, startModelVersionSidecar(t, "current-model-sha"))
	id := seedVocalGateRejection(t, ctx, dbPath, outdir, "0.0.1-stale")

	var app bytes.Buffer
	if code := runReconcileInstrumentalRecalibrate(ctx, &app, ScanReconcileInstrumentalRecalibrateCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, app.String())
	}
	if !strings.Contains(app.String(), "reset-stale=1") {
		t.Errorf("output should report the reset:\n%s", app.String())
	}

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open verify: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	var result *int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT instrumental_result FROM work_queue WHERE id = ?`, id,
	).Scan(&result); err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if result != nil {
		t.Errorf("instrumental_result = %v; want NULL (reset to never-classified)", *result)
	}
}

// TestRunReconcileInstrumentalRecalibrate_ConfigLoadFailure covers
// openQueueEnv's config.Load error branch: a config file that exists but
// fails to decode.
func TestRunReconcileInstrumentalRecalibrate_ConfigLoadFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("not valid toml [[["), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}

	var buf bytes.Buffer
	code := runReconcileInstrumentalRecalibrate(ctx, &buf, ScanReconcileInstrumentalRecalibrateCmd{ConfigPath: cfgPath})
	if code != 1 {
		t.Fatalf("exit=%d; want 1 for an undecodable config", code)
	}
}

// TestRunReconcileInstrumentalRecalibrate_LibraryNotFound covers
// openQueueEnv/resolveEnvLibrary's not-found error branch.
func TestRunReconcileInstrumentalRecalibrate_LibraryNotFound(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRecalibrateCfg(t, cfgPath, dbPath)
	// db.Open creates the schema so resolveLibrary can run.
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	_ = sqlDB.Close()

	var buf bytes.Buffer
	code := runReconcileInstrumentalRecalibrate(ctx, &buf, ScanReconcileInstrumentalRecalibrateCmd{ConfigPath: cfgPath, Library: "no-such-library"})
	if code != 1 {
		t.Fatalf("exit=%d; want 1 for an unknown --library; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "not found") {
		t.Errorf("want 'not found' message; got: %s", buf.String())
	}
}

// TestRunReconcileInstrumentalRecalibrate_ScopedToLibrary covers
// resolveEnvLibrary's success branch (id + label resolved) and confirms the
// scoping actually narrows the candidate set: a row in a different library is
// left untouched.
func TestRunReconcileInstrumentalRecalibrate_ScopedToLibrary(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeRecalibrateCfg(t, cfgPath, dbPath)
	id := seedVocalGateRejection(t, ctx, dbPath, outdir, version)

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	var libID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO libraries (path, name) VALUES (?, ?) RETURNING id`, outdir, "music-lib",
	).Scan(&libID); err != nil {
		t.Fatalf("insert library: %v", err)
	}
	_ = sqlDB.Close()

	var buf bytes.Buffer
	code := runReconcileInstrumentalRecalibrate(ctx, &buf, ScanReconcileInstrumentalRecalibrateCmd{ConfigPath: cfgPath, Library: "music-lib"})
	if code != 0 {
		t.Fatalf("exit=%d; want 0; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "library \"music-lib\"") {
		t.Errorf("want the operator-facing library label in output; got: %s", buf.String())
	}
	// The row has no linked scan_results row for this library, so scoping to
	// it must exclude the row: nothing considered.
	if !strings.Contains(buf.String(), "0 vocal-gate-rejected row(s) considered") {
		t.Errorf("expected the library scoping to exclude the unlinked row; got: %s", buf.String())
	}

	sqlDB2, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open verify: %v", err)
	}
	defer func() { _ = sqlDB2.Close() }()
	var status string
	if err := sqlDB2.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if status != "deferred" {
		t.Errorf("status = %q; want deferred (row outside the scoped library must be untouched)", status)
	}
}

// TestRunReconcileInstrumentalRecalibrate_BackupOpenFailure covers Report's
// backup-file-open error branch: an unwritable --backup path must count as a
// per-row error (and thus a non-zero exit) rather than silently dropping the
// backup record.
func TestRunReconcileInstrumentalRecalibrate_BackupOpenFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeRecalibrateCfg(t, cfgPath, dbPath)
	seedVocalGateRejection(t, ctx, dbPath, outdir, version)

	// A backup path inside a non-existent directory: os.OpenFile fails.
	badBackup := filepath.Join(dir, "no-such-dir", "backup.jsonl")

	var buf bytes.Buffer
	code := runReconcileInstrumentalRecalibrate(ctx, &buf, ScanReconcileInstrumentalRecalibrateCmd{
		ConfigPath: cfgPath, Yes: true, Backup: badBackup,
	})
	if code != 1 {
		t.Fatalf("exit=%d; want 1 when the backup file cannot be opened; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "errors=1") {
		t.Errorf("want the backup-open failure counted as an error; got: %s", buf.String())
	}
}

// TestRunReconcileInstrumentalRecalibrate_BackupRecordsOutcome verifies the JSONL
// backup carries BOTH an intent record (before the mutation) and an outcome
// record (applied, after), so a restore can tell a committed change from a
// no-op (#515).
func TestRunReconcileInstrumentalRecalibrate_BackupRecordsOutcome(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeRecalibrateCfg(t, cfgPath, dbPath)
	id := seedVocalGateRejection(t, ctx, dbPath, outdir, version)
	backupPath := filepath.Join(dir, "backup.jsonl")

	var buf bytes.Buffer
	if code := runReconcileInstrumentalRecalibrate(ctx, &buf, ScanReconcileInstrumentalRecalibrateCmd{
		ConfigPath: cfgPath, Yes: true, Backup: backupPath,
	}); code != 0 {
		t.Fatalf("exit=%d\n%s", code, buf.String())
	}

	data, err := os.ReadFile(backupPath) //nolint:gosec // G304: test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var sawIntent, sawOutcome bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec struct {
			Type    string `json:"type"`
			QueueID int64  `json:"queue_id"`
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		if rec.QueueID != id {
			t.Fatalf("record for id=%d; want %d", rec.QueueID, id)
		}
		switch rec.Type {
		case "intent":
			sawIntent = true
		case "outcome":
			sawOutcome = true
			if rec.Outcome != "applied" {
				t.Errorf("outcome=%q; want applied", rec.Outcome)
			}
		default:
			t.Fatalf("unexpected record type %q", rec.Type)
		}
	}
	if !sawIntent || !sawOutcome {
		t.Fatalf("backup missing records: intent=%v outcome=%v\n%s", sawIntent, sawOutcome, data)
	}
}

// TestRunReconcileInstrumentalRecalibrate_AfterIDRejectedWithReverse verifies the
// forward-only --after-id cursor is refused (exit 1) when combined with
// --reverse, rather than silently ignored (#516).
func TestRunReconcileInstrumentalRecalibrate_AfterIDRejectedWithReverse(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRecalibrateCfg(t, cfgPath, dbPath)

	var buf bytes.Buffer
	code := runReconcileInstrumentalRecalibrate(ctx, &buf, ScanReconcileInstrumentalRecalibrateCmd{
		ConfigPath: cfgPath, Reverse: true, AfterID: 5,
	})
	if code != 1 {
		t.Fatalf("exit=%d; want 1 for --after-id with --reverse\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "not supported with --reverse") {
		t.Errorf("want a clear rejection message; got:\n%s", buf.String())
	}
}

// TestRunReconcileInstrumentalRecalibrate_ResumeCursorPrinted verifies that a
// forward --limit run that fills its cap prints the resume cursor so the operator
// can page past the rows just examined (#516).
func TestRunReconcileInstrumentalRecalibrate_ResumeCursorPrinted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	writeRecalibrateCfg(t, cfgPath, dbPath)
	id := seedVocalGateRejection(t, ctx, dbPath, outdir, version)

	// --limit 1 with one candidate: Total == limit, so the run may have left more
	// behind and must print the resume cursor pointing at the examined id.
	var buf bytes.Buffer
	if code := runReconcileInstrumentalRecalibrate(ctx, &buf, ScanReconcileInstrumentalRecalibrateCmd{
		ConfigPath: cfgPath, Yes: true, Limit: 1,
	}); code != 0 {
		t.Fatalf("exit=%d\n%s", code, buf.String())
	}
	want := "resume with --after-id=" + strconv.FormatInt(id, 10)
	if !strings.Contains(buf.String(), want) {
		t.Errorf("want %q in output; got:\n%s", want, buf.String())
	}
}
