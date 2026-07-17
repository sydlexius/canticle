package commands

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/doxazo-net/canticle/internal/db"
	"github.com/doxazo-net/canticle/internal/models"
	"github.com/doxazo-net/canticle/internal/queue"
)

// instrumentalClassifierResponse is a stub classifier body that passes all three
// detector gates. Every configured class must be PRESENT: the detector fails
// closed, so an absent vocal class means the vocal gate cannot run and the track
// is treated as not-instrumental (it will not silently pass a gate it could not
// evaluate). So the vocal classes appear with honest sub-threshold peaks rather
// than being omitted.
//
//   - music gate:  summed mean of {Music, Musical instrument} = 0.95 >= 0.90
//   - vocal gate:  every sung-vocal peak < 0.03
//   - speech gate: summed mean of {Speech} = 0.01 < 0.20
const instrumentalClassifierResponse = `{
  "mean": {"Music": 0.85, "Musical instrument": 0.10, "Speech": 0.01},
  "max":  {"Music": 0.99, "Musical instrument": 0.95, "Speech": 0.02,
           "Singing": 0.005, "Vocal music": 0.004, "Choir": 0.001,
           "A capella": 0.001, "Chant": 0.002, "Rapping": 0.001,
           "Child singing": 0.001, "Synthetic singing": 0.001,
           "Yodeling": 0.001, "Humming": 0.002}
}`

// seedUnclassifiedDeferred parks one row in the state issue #499 targets: deferred
// on a benign provider miss, never scored by the detector (instrumental_result
// NULL). Returns the row id.
func seedUnclassifiedDeferred(t *testing.T, ctx context.Context, dbPath, outdir string) int64 {
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
	return item.ID
}

// TestRunReconcileInstrumental_WritesMarkerForNeverScoredRow is the end-to-end
// #499 path: a deferred row the detector has never seen, which the (stub)
// classifier now calls instrumental, must under --yes get its marker written and
// the row completed -- with zero provider requests. The dry run must change
// nothing.
func TestRunReconcileInstrumental_WritesMarkerForNeverScoredRow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(instrumentalClassifierResponse))
	}))
	defer srv.Close()

	cfgPath := filepath.Join(dir, "config.toml")
	writeReconcileCfg(t, cfgPath, dbPath, srv.URL, fakeFFmpegCmd(t))
	id := seedUnclassifiedDeferred(t, ctx, dbPath, outdir)
	markerPath := filepath.Join(outdir, "song.txt")

	// Dry run: must change nothing on disk or in the row.
	var dry bytes.Buffer
	if code := runReconcileInstrumental(ctx, &dry, ScanReconcileInstrumentalCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("dry-run exit=%d out=%s", code, dry.String())
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write a marker; stat err=%v", err)
	}
	if !strings.Contains(dry.String(), "would mark") {
		t.Errorf("dry-run output missing 'would mark':\n%s", dry.String())
	}

	// Apply.
	var app bytes.Buffer
	if code := runReconcileInstrumental(ctx, &app, ScanReconcileInstrumentalCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, app.String())
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("apply must write the instrumental marker: %v", err)
	}

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open verify: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	var status string
	var result *int
	var outcome *string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, instrumental_result, outcome_type FROM work_queue WHERE id = ?`, id,
	).Scan(&status, &result, &outcome); err != nil {
		t.Fatalf("verify row: %v", err)
	}
	if status != "done" {
		t.Errorf("status = %q; want done (the row is settled, not left deferred)", status)
	}
	if result == nil || *result != 1 {
		t.Errorf("instrumental_result = %v; want 1 (stamped so it is never re-checked blindly)", result)
	}
	if outcome == nil || *outcome != "instrumental" {
		t.Errorf("outcome_type = %v; want instrumental (so reports classify it)", outcome)
	}
}

// TestRunReconcileInstrumental_HonorsPerItemOptOut: a row whose enqueue-time
// decision explicitly disabled detection must stay untouched. This command is a
// backfill for rows nobody ever looked at, not a license to override a decision
// the operator already made.
func TestRunReconcileInstrumental_HonorsPerItemOptOut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(instrumentalClassifierResponse))
	}))
	defer srv.Close()

	cfgPath := filepath.Join(dir, "config.toml")
	writeReconcileCfg(t, cfgPath, dbPath, srv.URL, fakeFFmpegCmd(t))

	srcPath := filepath.Join(outdir, "song.flac")
	if err := os.WriteFile(srcPath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	optOut := false
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:              models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:             outdir,
		Filename:           "song.lrc",
		SourcePath:         srcPath,
		OutputPaths:        []models.OutputPath{{Outdir: outdir, Filename: "song.flac"}},
		DetectInstrumental: &optOut, // explicitly opted out at enqueue
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
	_ = sqlDB.Close()

	var buf bytes.Buffer
	if code := runReconcileInstrumental(ctx, &buf, ScanReconcileInstrumentalCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if _, err := os.Stat(filepath.Join(outdir, "song.txt")); !os.IsNotExist(err) {
		t.Errorf("opted-out row must not get a marker; stat err=%v", err)
	}
	if !strings.Contains(buf.String(), "detect-off=1") {
		t.Errorf("output should report the opt-out skip:\n%s", buf.String())
	}
}

// notInstrumentalClassifierResponse fails the vocal gate: a strong sung-vocal
// peak. All configured classes are present so every gate can actually run.
const notInstrumentalClassifierResponse = `{
  "mean": {"Music": 0.85, "Musical instrument": 0.10, "Speech": 0.01},
  "max":  {"Music": 0.99, "Musical instrument": 0.95, "Speech": 0.02,
           "Singing": 0.80, "Vocal music": 0.60, "Choir": 0.001,
           "A capella": 0.001, "Chant": 0.002, "Rapping": 0.001,
           "Child singing": 0.001, "Synthetic singing": 0.001,
           "Yodeling": 0.001, "Humming": 0.002}
}`

// TestRunReconcileInstrumental_NotInstrumentalStampsAndLeavesDeferred: when the
// detector disagrees, no marker is written and the row stays deferred (the
// provider may still find lyrics for it) -- but the negative verdict is stamped
// so a later run does not re-pay the inference and the row is distinguishable
// from "never detected".
func TestRunReconcileInstrumental_NotInstrumentalStampsAndLeavesDeferred(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(notInstrumentalClassifierResponse))
	}))
	defer srv.Close()

	cfgPath := filepath.Join(dir, "config.toml")
	writeReconcileCfg(t, cfgPath, dbPath, srv.URL, fakeFFmpegCmd(t))
	id := seedUnclassifiedDeferred(t, ctx, dbPath, outdir)

	var buf bytes.Buffer
	if code := runReconcileInstrumental(ctx, &buf, ScanReconcileInstrumentalCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if _, err := os.Stat(filepath.Join(outdir, "song.txt")); !os.IsNotExist(err) {
		t.Errorf("a vocal track must never get an instrumental marker; stat err=%v", err)
	}
	if !strings.Contains(buf.String(), "not-instrumental=1") {
		t.Errorf("output should report the disagreement:\n%s", buf.String())
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
	if status != "deferred" {
		t.Errorf("status = %q; want deferred (a provider may still find lyrics)", status)
	}
	if result == nil || *result != 0 {
		t.Errorf("instrumental_result = %v; want 0 stamped (distinguishable from never-detected)", result)
	}
}

// TestRunReconcileInstrumental_LimitReportsWhatItLeftBehind: a capped run must
// say what it did not examine. A silent truncation reads as "covered
// everything" when it did not.
func TestRunReconcileInstrumental_LimitReportsWhatItLeftBehind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(instrumentalClassifierResponse))
	}))
	defer srv.Close()
	cfgPath := filepath.Join(dir, "config.toml")
	writeReconcileCfg(t, cfgPath, dbPath, srv.URL, fakeFFmpegCmd(t))

	// Two never-classified deferred rows; --limit=1 must examine one and say so.
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	for _, title := range []string{"one", "two"} {
		src := filepath.Join(outdir, title+".flac")
		if err := os.WriteFile(src, []byte("audio"), 0o600); err != nil {
			t.Fatalf("write source: %v", err)
		}
		item, err := q.Enqueue(ctx, models.Inputs{
			Track:       models.Track{ArtistName: "Artist", TrackName: title},
			Outdir:      outdir,
			Filename:    title + ".lrc",
			SourcePath:  src,
			OutputPaths: []models.OutputPath{{Outdir: outdir, Filename: title + ".flac"}},
		}, queue.PriorityScan)
		if err != nil {
			t.Fatalf("enqueue %s: %v", title, err)
		}
		if _, err := q.Dequeue(ctx); err != nil {
			t.Fatalf("dequeue %s: %v", title, err)
		}
		if _, err := q.Defer(ctx, item.ID, 0, nil); err != nil {
			t.Fatalf("defer %s: %v", title, err)
		}
	}
	_ = sqlDB.Close()

	var buf bytes.Buffer
	if code := runReconcileInstrumental(ctx, &buf, ScanReconcileInstrumentalCmd{ConfigPath: cfgPath, Limit: 1}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "2 never-classified deferred row(s) total; 1 candidate") {
		t.Errorf("output should report the full backlog alongside the capped candidate set:\n%s", out)
	}
	if !strings.Contains(out, "left unexamined") {
		t.Errorf("a capped run must say what it left behind; got:\n%s", out)
	}
}

// TestRunReconcileInstrumental_DetectorNotConfigured: with no classifier_url the
// command is inert and must say so rather than silently reporting zero work.
func TestRunReconcileInstrumental_DetectorNotConfigured(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.toml")
	writeReconcileCfg(t, cfgPath, dbPath, "", fakeFFmpegCmd(t))

	var buf bytes.Buffer
	if code := runReconcileInstrumental(ctx, &buf, ScanReconcileInstrumentalCmd{ConfigPath: cfgPath}); code != 1 {
		t.Fatalf("exit=%d; want 1 when no classifier is configured. out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "instrumental detector is not configured") {
		t.Errorf("must name the missing classifier; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "backfill instrumental verdicts") {
		t.Errorf("message should name this command, not the sibling reconcile; got:\n%s", buf.String())
	}
}
