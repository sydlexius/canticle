package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

func getLibraryForTest(t *testing.T, ctx context.Context, dbPath string, id int64) models.Library {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	lib, err := library.New(sqlDB).Get(ctx, id)
	if err != nil {
		t.Fatalf("get library: %v", err)
	}
	return lib
}

func TestRunLibrarySettingsFlags(t *testing.T) {
	bp := func(v bool) *bool { return &v }
	isolateCommandsEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state", "test.db")
	cfg := writeCommandsConfig(t, dbPath)
	libPath := filepath.Join(dir, "music")
	if err := os.Mkdir(libPath, 0o750); err != nil {
		t.Fatalf("mkdir library: %v", err)
	}

	var out bytes.Buffer
	code := runLibrary(ctx, &out, LibraryCmd{Add: &LibraryAddCmd{
		Path:               libPath,
		Name:               "Music",
		Enrich:             bp(false),
		DetectInstrumental: bp(true),
		ConfigPath:         cfg,
	}})
	if code != 0 {
		t.Fatalf("library add exit code = %d; want 0", code)
	}

	lib := getLibraryForTest(t, ctx, dbPath, 1)
	if lib.EnrichRecording == nil || *lib.EnrichRecording {
		t.Fatalf("EnrichRecording = %v; want false", lib.EnrichRecording)
	}
	if lib.DetectInstrumental == nil || !*lib.DetectInstrumental {
		t.Fatalf("DetectInstrumental = %v; want true", lib.DetectInstrumental)
	}

	// Update with only --enrich (no --path/--name) is allowed and changes only
	// the enrich column; detect stays unchanged.
	out.Reset()
	code = runLibrary(ctx, &out, LibraryCmd{Update: &LibraryUpdateCmd{
		ID:         1,
		Enrich:     bp(true),
		ConfigPath: cfg,
	}})
	if code != 0 {
		t.Fatalf("library update --enrich exit code = %d; want 0", code)
	}
	lib = getLibraryForTest(t, ctx, dbPath, 1)
	if lib.EnrichRecording == nil || !*lib.EnrichRecording {
		t.Fatalf("after update EnrichRecording = %v; want true", lib.EnrichRecording)
	}
	if lib.DetectInstrumental == nil || !*lib.DetectInstrumental {
		t.Fatalf("after update DetectInstrumental = %v; want true (unchanged)", lib.DetectInstrumental)
	}
}
