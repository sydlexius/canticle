package commands

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/realign"
)

// realignTestSetup opens a DB with one library rooted at a canonical temp dir.
func realignTestSetup(t *testing.T) (context.Context, *sql.DB, models.Library, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	sqlDB, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	lib, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	return ctx, sqlDB, lib, root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestReactiveRealign_MovesOrphanInScannedDir: reactiveRealign re-attaches an
// orphaned sidecar in a directory a scan touched (heuristic tier, auto-applied).
func TestReactiveRealign_MovesOrphanInScannedDir(t *testing.T) {
	ctx, sqlDB, lib, root := realignTestSetup(t)
	audio := filepath.Join(root, "Artist", "Song Title.flac")
	orphan := filepath.Join(root, "Artist", "Song Titl.lrc")
	writeFile(t, audio, "x")
	writeFile(t, orphan, "[ti:Song Title]\n[00:01.00]x\n")

	cfg := config.RealignConfig{IdentityKeys: []string{"mbid", "isrc"}, MinConfidence: 0.75, AutoApplyHeuristic: true}
	r := realign.New(library.New(sqlDB), cfg)
	backup := filepath.Join(t.TempDir(), "b.jsonl")

	reactiveRealign(ctx, r, backup, lib, []models.ScanResult{{FilePath: audio}})

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan should have moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Artist", "Song Title.lrc")); err != nil {
		t.Errorf("realigned sidecar missing: %v", err)
	}
}

// TestReactiveRealign_NilRealignerIsNoop: a nil realigner does nothing.
func TestReactiveRealign_NilRealignerIsNoop(t *testing.T) {
	ctx, _, lib, root := realignTestSetup(t)
	orphan := filepath.Join(root, "Artist", "x.lrc")
	writeFile(t, orphan, "[00:01.00]x\n")
	reactiveRealign(ctx, nil, "", lib, []models.ScanResult{{FilePath: filepath.Join(root, "Artist", "y.flac")}})
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("nil realigner must not touch anything: %v", err)
	}
}

// TestServeRealigner_Gating covers the enabled / on_scan gating that selects
// whether a scan trigger gets a realigner.
func TestServeRealigner_Gating(t *testing.T) {
	_, sqlDB, _, _ := realignTestSetup(t)
	cases := []struct {
		name          string
		enabled       bool
		onScan        bool
		requireOnScan bool
		wantNil       bool
	}{
		{"disabled", false, false, false, true},
		{"enabled watcher (no on_scan needed)", true, false, false, false},
		{"enabled but on_scan required and off", true, false, true, true},
		{"enabled and on_scan", true, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Realign.Enabled = tc.enabled
			cfg.Realign.OnScan = tc.onScan
			r, backup := serveRealigner(sqlDB, cfg, tc.requireOnScan)
			if (r == nil) != tc.wantNil {
				t.Errorf("realigner nil = %v; want %v", r == nil, tc.wantNil)
			}
			if tc.wantNil && backup != "" {
				t.Errorf("disabled path should return empty backup path, got %q", backup)
			}
		})
	}
}

// TestServerRealigner_RealignDir exercises the webhook adapter end to end.
func TestServerRealigner_RealignDir(t *testing.T) {
	ctx, sqlDB, _, root := realignTestSetup(t)
	audio := filepath.Join(root, "Artist", "Song Title.flac")
	orphan := filepath.Join(root, "Artist", "Song Titl.lrc")
	writeFile(t, audio, "x")
	writeFile(t, orphan, "[ti:Song Title]\n[00:01.00]x\n")

	cfg := config.RealignConfig{IdentityKeys: []string{"mbid", "isrc"}, MinConfidence: 0.75, AutoApplyHeuristic: true}
	adapter := serverRealigner{realigner: realign.New(library.New(sqlDB), cfg), backupPath: filepath.Join(t.TempDir(), "b.jsonl")}
	if err := adapter.RealignDir(ctx, filepath.Join(root, "Artist")); err != nil {
		t.Fatalf("RealignDir: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan should have moved via the webhook adapter: %v", err)
	}
}
