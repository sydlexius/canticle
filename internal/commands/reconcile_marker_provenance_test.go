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
		SourcePath:  filepath.Join(outdir, "song.flac"),
		OutputPaths: []models.OutputPath{{Outdir: outdir, Filename: "song.lrc"}},
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
	// Bare marker on disk (old writeInstrumental format).
	if err := os.WriteFile(filepath.Join(outdir, "song.txt"), []byte(lyrics.InstrumentalMarker+"\n"), 0o644); err != nil {
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
