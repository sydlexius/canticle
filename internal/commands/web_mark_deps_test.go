package commands

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/selfwrite"
)

// TestWebMarkDeps pins what webMarkDeps promises: with a database path it returns
// fully wired services carrying that path (backups land beside the database); with
// no path it returns the disabled form (empty DBPath, which makes the routes 404).
func TestWebMarkDeps(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "serve.db")
	sqlDB, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	reg := selfwrite.New(0)

	got := webMarkDeps(sqlDB, dbPath, []string{t.TempDir()}, reg)
	if got.DBPath != dbPath {
		t.Errorf("DBPath = %q, want %q", got.DBPath, dbPath)
	}
	if got.DB != sqlDB || got.Instrumental == nil || got.Blocks == nil {
		t.Errorf("deps not fully wired: %+v", got)
	}

	off := webMarkDeps(sqlDB, "", nil, reg)
	if off.DBPath != "" {
		t.Errorf("disabled DBPath = %q, want empty", off.DBPath)
	}
}
