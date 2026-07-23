package commands

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// writeReconcileCfg writes a minimal config pointing the detector at a stub
// classifier and a fake ffmpeg so runScanReconcile constructs a working detector.
func writeReconcileCfg(t *testing.T, path, dbPath, classifierURL, ffmpegPath string) {
	t.Helper()
	escape := func(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }
	// ffmpeg_path goes under [verification]: resolveFFmpeg prefers it, and a blank
	// value is re-defaulted to "ffmpeg" (a real binary on PATH), which would shadow
	// an [instrumental_detector] override.
	content := "[db]\n" +
		"path = \"" + escape(dbPath) + "\"\n\n" +
		"[verification]\n" +
		"ffmpeg_path = \"" + escape(ffmpegPath) + "\"\n\n" +
		"[instrumental_detector]\n" +
		"enabled = true\n" +
		"classifier_url = \"" + escape(classifierURL) + "\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeReconcileCfg: %v", err)
	}
}

// fakeFFmpegCmd writes a stub ffmpeg that writes a byte to its last argument (the
// sample output path), so the detector's sampling step succeeds without real
// ffmpeg. Mirrors the detector package's test fake.
func fakeFFmpegCmd(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\nlast=''\nfor arg do\n  last=\"$arg\"\ndone\nprintf 'sampled audio' > \"$last\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

// TestRunScanReconcile_ClearsStaleMarker is the end-to-end reconcile path: a row
// flagged instrumental whose source the (stub) detector now classifies as NOT
// instrumental must, under --yes, have its exact-marker .txt deleted and its
// work_queue row re-queued (deferred, verdict NULL), with a JSONL backup written -
// and the dry-run must change nothing.
func TestRunScanReconcile_ClearsStaleMarker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir music: %v", err)
	}
	srcPath := filepath.Join(outdir, "song.flac")
	if err := os.WriteFile(srcPath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	markerPath := filepath.Join(outdir, "song.txt")
	if err := os.WriteFile(markerPath, []byte(lyrics.InstrumentalMarker+"\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// Classifier returns an empty max map -> the detector degrades to NOT
	// instrumental, producing the disagreement reconcile acts on.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"mean":{"Speech":0.5},"max":{}}`))
	}))
	defer srv.Close()

	cfgPath := filepath.Join(dir, "config.toml")
	writeReconcileCfg(t, cfgPath, dbPath, srv.URL, fakeFFmpegCmd(t))

	// Seed a completed, instrumental-tagged row carrying the marker. detector_version
	// "old" differs from the current build, so the default narrowed prefilter selects it.
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	inputs := models.Inputs{
		Track:       models.Track{ArtistName: "A", TrackName: "Song"},
		SourcePath:  srcPath,
		OutputPaths: []models.OutputPath{{Outdir: outdir, Filename: "song.flac"}},
	}
	if _, err := q.Enqueue(ctx, inputs, queue.PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := q.SetInstrumentalResult(ctx, item.ID, 1, queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.01, SpeechMean: 0.002, VocalClass: "Singing", DetectorVersion: "old"}); err != nil {
		t.Fatalf("SetInstrumentalResult: %v", err)
	}
	// Reconcile only touches completed instrumental rows.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'done', completed_at = ? WHERE id = ?`, "2026-06-25T00:00:00Z", item.ID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	_ = sqlDB.Close()

	// Dry run: must not touch the marker.
	var dry bytes.Buffer
	if code := runScanReconcile(ctx, &dry, ScanReconcileCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("dry-run exit=%d out=%s", code, dry.String())
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("dry-run must not delete the marker: %v", err)
	}
	if !strings.Contains(dry.String(), "would clear") {
		t.Errorf("dry-run output missing 'would clear':\n%s", dry.String())
	}

	// Apply.
	var app bytes.Buffer
	if code := runScanReconcile(ctx, &app, ScanReconcileCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, app.String())
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("marker should be deleted after apply; stat err=%v", err)
	}
	if !strings.Contains(app.String(), "rows-reset=1") {
		t.Errorf("apply output missing rows-reset=1:\n%s", app.String())
	}

	// Verify the row was re-queued and its verdict cleared.
	sqlDB2, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open verify: %v", err)
	}
	defer sqlDB2.Close() //nolint:errcheck // test cleanup
	var status string
	var instResult sql.NullInt64
	if err := sqlDB2.QueryRowContext(ctx, `SELECT status, instrumental_result FROM work_queue WHERE id = ?`, item.ID).Scan(&status, &instResult); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "deferred" {
		t.Errorf("status = %q; want deferred", status)
	}
	if instResult.Valid {
		t.Errorf("instrumental_result not NULL after reconcile: %v", instResult)
	}

	// A JSONL backup must exist.
	matches, _ := filepath.Glob(filepath.Join(dir, "reconcile-backup-*.jsonl"))
	if len(matches) == 0 {
		t.Errorf("no reconcile backup file written in %s", dir)
	}
}

// seedInstrumentalRow enqueues + dequeues a row and stamps it instrumental with the
// given telemetry version, returning its id. Opens and closes its own DB handle.
func seedInstrumentalRow(t *testing.T, ctx context.Context, dbPath string, inputs models.Inputs, detVer string) int64 {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	if _, err := q.Enqueue(ctx, inputs, queue.PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := q.SetInstrumentalResult(ctx, item.ID, 1, queue.InstrumentalTelemetry{MusicSum: 0.95, VocalPeak: 0.01, SpeechMean: 0.002, VocalClass: "Singing", DetectorVersion: detVer}); err != nil {
		t.Fatalf("SetInstrumentalResult: %v", err)
	}
	// Reconcile only touches completed instrumental rows.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'done', completed_at = ? WHERE id = ?`, "2026-06-25T00:00:00Z", item.ID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	return item.ID
}

// TestRunScanReconcile_DetectorNotConfigured: with no classifier_url, reconcile
// reports the detector is not configured and exits 1 (before resolving ffmpeg).
func TestRunScanReconcile_DetectorNotConfigured(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := "[db]\npath = \"" + strings.ReplaceAll(filepath.Join(dir, "test.db"), `\`, `\\`) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var buf bytes.Buffer
	if code := runScanReconcile(ctx, &buf, ScanReconcileCmd{ConfigPath: cfgPath}); code != 1 {
		t.Fatalf("exit=%d want 1; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "not configured") {
		t.Errorf("want 'not configured' message; got: %s", buf.String())
	}
}

// TestRunScanReconcile_LibraryNotFound: an unknown --library exits 1.
func TestRunScanReconcile_LibraryNotFound(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeReconcileCfg(t, cfgPath, filepath.Join(dir, "test.db"), "http://127.0.0.1:1", fakeFFmpegCmd(t))
	// db.Open creates the schema so resolveLibrary can run.
	sqlDB, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	_ = sqlDB.Close()
	var buf bytes.Buffer
	if code := runScanReconcile(ctx, &buf, ScanReconcileCmd{ConfigPath: cfgPath, Library: "no-such-library"}); code != 1 {
		t.Fatalf("exit=%d want 1; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "not found") {
		t.Errorf("want 'not found' message; got: %s", buf.String())
	}
}

// TestRunScanReconcile_ConfirmedAllMode: when the detector still classifies the
// track as instrumental, --all re-infers it, the verdict is confirmed, and nothing
// is deleted or re-queued.
func TestRunScanReconcile_ConfirmedAllMode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srcPath := filepath.Join(outdir, "song.flac")
	_ = os.WriteFile(srcPath, []byte("audio"), 0o600)
	markerPath := filepath.Join(outdir, "song.txt")
	_ = os.WriteFile(markerPath, []byte(lyrics.InstrumentalMarker+"\n"), 0o600)

	// Classifier returns instrumental: high music, a present-but-low vocal peak.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"mean":{"Music":0.95},"max":{"Music":1.0,"Singing":0.01}}`))
	}))
	defer srv.Close()
	cfgPath := filepath.Join(dir, "config.toml")
	writeReconcileCfg(t, cfgPath, dbPath, srv.URL, fakeFFmpegCmd(t))
	seedInstrumentalRow(t, ctx, dbPath, models.Inputs{
		Track:       models.Track{ArtistName: "A", TrackName: "Song"},
		SourcePath:  srcPath,
		OutputPaths: []models.OutputPath{{Outdir: outdir, Filename: "song.flac"}},
	}, "current")

	var buf bytes.Buffer
	if code := runScanReconcile(ctx, &buf, ScanReconcileCmd{ConfigPath: cfgPath, All: true, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("confirmed row's marker must NOT be deleted: %v", err)
	}
	if !strings.Contains(buf.String(), "confirmed=1") || !strings.Contains(buf.String(), "rows-reset=0") {
		t.Errorf("want confirmed=1 rows-reset=0; got: %s", buf.String())
	}
}

// TestRunScanReconcile_StampsTelemetryOnConfirm covers #557: a row decided
// before migration 025 carries instrumental_result=1 with NULL telemetry, and
// every re-decision path is arithmetic over those scores, so it is skipped by
// construction and never revisited. Reconcile re-runs live detection against
// exactly these rows, so its confirm branch is the one place the missing
// evidence can be recovered -- it previously just counted them and moved on.
//
// Without this test the wiring is unguarded: deleting the stamp call leaves
// every other reconcile test green.
func TestRunScanReconcile_StampsTelemetryOnConfirm(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srcPath := filepath.Join(outdir, "song.flac")
	_ = os.WriteFile(srcPath, []byte("audio"), 0o600)
	_ = os.WriteFile(filepath.Join(outdir, "song.txt"), []byte(lyrics.InstrumentalMarker+"\n"), 0o600)

	// Classifier confirms instrumental, so the run takes the confirm branch. Each
	// of the three scores is a DISTINCT non-zero value (music 0.95, speech 0.03,
	// vocal peak 0.01) so every telemetry field is independently constrained --
	// with a zero or shared value, wiring one field from another's source would
	// satisfy the assertions anyway. Speech stays under the 0.2 gate so the
	// instrumental verdict still holds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"mean":{"Music":0.95,"Speech":0.03},"max":{"Music":1.0,"Singing":0.01}}`))
	}))
	defer srv.Close()
	cfgPath := filepath.Join(dir, "config.toml")
	writeReconcileCfg(t, cfgPath, dbPath, srv.URL, fakeFFmpegCmd(t))
	id := seedInstrumentalRow(t, ctx, dbPath, models.Inputs{
		Track:       models.Track{ArtistName: "A", TrackName: "Song"},
		SourcePath:  srcPath,
		OutputPaths: []models.OutputPath{{Outdir: outdir, Filename: "song.flac"}},
	}, "current")

	// Reduce the row to the pre-025 shape the issue describes: the verdict
	// persisted, the evidence behind it not.
	clearTelemetry(t, ctx, dbPath, id)

	// DRY RUN FIRST. The command's contract is that --yes changes only whether the
	// write happens, never which rows are reported, so the dry run must report the
	// same telemetry-stamped count the apply does -- and must write nothing.
	var dry bytes.Buffer
	if code := runScanReconcile(ctx, &dry, ScanReconcileCmd{ConfigPath: cfgPath, All: true}); code != 0 {
		t.Fatalf("dry-run exit=%d out=%s", code, dry.String())
	}
	if !strings.Contains(dry.String(), "telemetry-stamped=1") {
		t.Errorf("dry run must PREVIEW the stamp it would make; want telemetry-stamped=1, got: %s", dry.String())
	}
	if m, _, _, _ := readTelemetry(t, ctx, dbPath, id); m != nil {
		t.Errorf("dry run wrote telemetry (music_sum=%v); it must write nothing", *m)
	}

	var buf bytes.Buffer
	if code := runScanReconcile(ctx, &buf, ScanReconcileCmd{ConfigPath: cfgPath, All: true, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "telemetry-stamped=1") {
		t.Errorf("want telemetry-stamped=1 in the summary; got: %s", buf.String())
	}

	music, vocal, speech, dv := readTelemetry(t, ctx, dbPath, id)
	if music == nil || vocal == nil || speech == nil {
		t.Fatalf("telemetry still NULL after confirm: music=%v vocal=%v speech=%v", music, vocal, speech)
	}
	// The scores must be the ones this detection produced, not placeholders:
	// the classifier above reports mean Music 0.95 and max Singing 0.01.
	if *music != 0.95 {
		t.Errorf("music_sum = %v; want 0.95 (the live detection's score)", *music)
	}
	if *vocal != 0.01 {
		t.Errorf("vocal_peak = %v; want 0.01 (the live detection's score)", *vocal)
	}
	// Asserted exactly, and distinct from the other two, so a field wired from the
	// wrong source on the Result cannot pass.
	if *speech != 0.03 {
		t.Errorf("speech_mean = %v; want 0.03 (the live detection's score)", *speech)
	}
	if dv == "" {
		t.Error("detector_version is empty; a stamped row must record which detector confirmed it")
	}
}

// clearTelemetry reduces a row to the pre-migration-025 shape: an instrumental
// verdict with no evidence behind it.
func clearTelemetry(t *testing.T, ctx context.Context, dbPath string, id int64) {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET music_sum = NULL, vocal_peak = NULL, speech_mean = NULL,
             vocal_class = NULL, detector_version = NULL WHERE id = ?`, id); err != nil {
		t.Fatalf("clear telemetry: %v", err)
	}
}

func readTelemetry(t *testing.T, ctx context.Context, dbPath string, id int64) (music, vocal, speech *float64, dv string) {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	var dvNull *string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT music_sum, vocal_peak, speech_mean, detector_version FROM work_queue WHERE id = ?`, id,
	).Scan(&music, &vocal, &speech, &dvNull); err != nil {
		t.Fatalf("read telemetry: %v", err)
	}
	if dvNull != nil {
		dv = *dvNull
	}
	return music, vocal, speech, dv
}

// TestRunScanReconcile_SkipsNoSourceAndNoMarker: a row with no source path is
// skipped before detection; a disagreeing row with no on-disk marker is left
// untouched (never reset on the basis of a missing marker).
func TestRunScanReconcile_SkipsNoSourceAndNoMarker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	outdir := filepath.Join(dir, "music")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srcPath := filepath.Join(outdir, "song.flac")
	_ = os.WriteFile(srcPath, []byte("audio"), 0o600)
	// Deliberately do NOT create song.txt -> disagreement but no marker.

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"mean":{"Speech":0.5},"max":{}}`)) // not instrumental
	}))
	defer srv.Close()
	cfgPath := filepath.Join(dir, "config.toml")
	writeReconcileCfg(t, cfgPath, dbPath, srv.URL, fakeFFmpegCmd(t))

	// Row 1: no source path -> skipped before detection.
	seedInstrumentalRow(t, ctx, dbPath, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "NoSrc"}}, "old")
	// Row 2: source present, disagreement, but no marker file on disk.
	noMarkerID := seedInstrumentalRow(t, ctx, dbPath, models.Inputs{
		Track:       models.Track{ArtistName: "B", TrackName: "NoMarker"},
		SourcePath:  srcPath,
		OutputPaths: []models.OutputPath{{Outdir: outdir, Filename: "song.flac"}},
	}, "old")

	var buf bytes.Buffer
	if code := runScanReconcile(ctx, &buf, ScanReconcileCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "no-source=1") || !strings.Contains(buf.String(), "no-marker=1") {
		t.Errorf("want no-source=1 no-marker=1; got: %s", buf.String())
	}
	// The no-marker row must NOT have been reset.
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open verify: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	var instResult sql.NullInt64
	if err := sqlDB.QueryRowContext(ctx, `SELECT instrumental_result FROM work_queue WHERE id = ?`, noMarkerID).Scan(&instResult); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !instResult.Valid || instResult.Int64 != 1 {
		t.Errorf("no-marker row must remain instrumental_result=1; got %v", instResult)
	}
}

// TestExactInstrumentalSidecars verifies the deletion-safety filter: only files
// whose content is exactly the instrumental marker are returned; real unsynced
// lyrics and missing files are excluded.
func TestExactInstrumentalSidecars(t *testing.T) {
	dir := t.TempDir()
	markerDir := filepath.Join(dir, "marker")
	lyricsDir := filepath.Join(dir, "lyrics")
	missingDir := filepath.Join(dir, "missing")
	for _, d := range []string{markerDir, lyricsDir, missingDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// Exact marker (with trailing newline, as the writer emits it).
	if err := os.WriteFile(filepath.Join(markerDir, "t.txt"), []byte(lyrics.InstrumentalMarker+"\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	// Genuine unsynced lyrics .txt - must NOT be returned.
	if err := os.WriteFile(filepath.Join(lyricsDir, "t.txt"), []byte("These are real lyrics\nline two\n"), 0o600); err != nil {
		t.Fatalf("write lyrics: %v", err)
	}

	item := queue.WorkItem{
		ID: 1,
		Inputs: models.Inputs{
			Track: models.Track{ArtistName: "A", TrackName: "T"},
			OutputPaths: []models.OutputPath{
				{Outdir: markerDir, Filename: "t.flac"},
				{Outdir: lyricsDir, Filename: "t.flac"},
				{Outdir: missingDir, Filename: "t.flac"},
			},
		},
	}
	got := exactInstrumentalSidecars(item)
	want := filepath.Join(markerDir, "t.txt")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("exactInstrumentalSidecars = %v; want exactly [%s]", got, want)
	}
}

// TestIsExactInstrumentalMarker covers the strict marker match.
func TestIsExactInstrumentalMarker(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"exact with newline", write("a.txt", lyrics.InstrumentalMarker+"\n"), true},
		{"exact no newline", write("b.txt", lyrics.InstrumentalMarker), true},
		{"exact trailing spaces", write("c.txt", lyrics.InstrumentalMarker+"  \n"), true},
		{"real lyrics", write("d.txt", "real lyrics here"), false},
		{"marker with prefix", write("e.txt", "x"+lyrics.InstrumentalMarker), false},
		{"missing", filepath.Join(dir, "nope.txt"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExactInstrumentalMarker(tc.path); got != tc.want {
				t.Errorf("isExactInstrumentalMarker(%s) = %v; want %v", tc.name, got, tc.want)
			}
		})
	}
}
