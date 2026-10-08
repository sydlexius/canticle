package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/models"
)

// seedMultiEntryRow is the #1430 shape: output_paths holds a stale entry and
// the row's own valid one.
func seedMultiEntryRow(t *testing.T, ctx context.Context, dbPath, newPath, staleDir string) int64 {
	t.Helper()
	id := seedReconcilePathsPreBrokenRow(t, ctx, dbPath, newPath, staleDir)
	raw, err := json.Marshal([]models.OutputPath{
		{Outdir: staleDir, Filename: filepath.Base(newPath)},
		{Outdir: filepath.Dir(newPath), Filename: filepath.Base(newPath)},
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET output_paths = ? WHERE id = ?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	return id
}

// reconcile-paths no longer reports a stale-plus-valid row as ambiguous: the dry
// run says it would repair it (aggregate-only) and changes nothing; --yes drops
// the stale entry.
func TestReconcilePaths_MultiEntryDryRunThenApply(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	newPath := filepath.Join(root, "ArtistMulti", "AlbumNew", "01. track.flac")
	staleDir := filepath.Join(root, "ArtistMulti", "AlbumOld")
	id := seedMultiEntryRow(t, ctx, dbPath, newPath, staleDir)

	var dry bytes.Buffer
	if code := runReconcilePaths(ctx, &dry, ScanReconcilePathsCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("dry exit=%d out=%s", code, dry.String())
	}
	if !strings.Contains(dry.String(), "would repair 1 work_queue row") || !strings.Contains(dry.String(), "0 ambiguous") {
		t.Errorf("dry run out = %s, want it to report one would-repair and no ambiguous skip", dry.String())
	}
	if strings.Contains(dry.String(), "ArtistMulti") {
		t.Errorf("dry run leaked an identifier: %s", dry.String())
	}
	if got := reconcilePathsWorkQueueOutputPaths(t, ctx, dbPath, id); len(got) != 2 {
		t.Fatalf("dry run changed output_paths: %+v", got)
	}

	var applied bytes.Buffer
	if code := runReconcilePaths(ctx, &applied, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, applied.String())
	}
	got := reconcilePathsWorkQueueOutputPaths(t, ctx, dbPath, id)
	if len(got) != 1 || got[0].Outdir != filepath.Dir(newPath) {
		t.Errorf("output_paths after apply = %+v, want only the valid entry", got)
	}
}

// A work item with no linked scan_results row gives the stale test no evidence,
// so its missing entry is retained and the CLI counts it as retained (#1430).
func TestReconcilePaths_MultiEntryWithoutLinkRowsIsRetained(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	newPath := filepath.Join(root, "ArtistMulti", "AlbumNew", "01. track.flac")
	staleDir := filepath.Join(root, "ArtistMulti", "AlbumOld")
	id := seedMultiEntryRow(t, ctx, dbPath, newPath, staleDir)
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM work_queue_scan_results WHERE work_queue_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	if code := runReconcilePaths(ctx, &out, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "retained 1 (") {
		t.Errorf("out = %s, want the unproven entry counted as retained", out.String())
	}
	if got := reconcilePathsWorkQueueOutputPaths(t, ctx, dbPath, id); len(got) != 2 {
		t.Errorf("output_paths = %+v, want both entries kept", got)
	}
}
