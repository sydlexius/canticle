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

// getLibraryForTest reads back library id 1, the only id any test in this
// file seeds.
func getLibraryForTest(t *testing.T, ctx context.Context, dbPath string) models.Library {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	lib, err := library.New(sqlDB).Get(ctx, 1)
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

	lib := getLibraryForTest(t, ctx, dbPath)
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
	lib = getLibraryForTest(t, ctx, dbPath)
	if lib.EnrichRecording == nil || !*lib.EnrichRecording {
		t.Fatalf("after update EnrichRecording = %v; want true", lib.EnrichRecording)
	}
	if lib.DetectInstrumental == nil || !*lib.DetectInstrumental {
		t.Fatalf("after update DetectInstrumental = %v; want true (unchanged)", lib.DetectInstrumental)
	}
}

// TestRunLibraryUpdate_LegacyRelativePathStaysMaintainable pins the fix for
// the regression the #643 hostile review caught: library.validate started
// hard-rejecting a relative path on Update as well as Add, but commands.go
// defaults the --path argument to the STORED path when --path is omitted, so
// a pre-existing relative-path library row (added on a released build before
// #643's Add-time check existed) could no longer be renamed or have its
// settings toggled without also supplying an absolute --path. Add rejecting a
// relative path is correct and stays; only Update of an operator-omitted
// path must tolerate a legacy relative row.
//
// The legacy row is seeded with a raw INSERT, bypassing repo.Add/validate
// entirely, the way such a row actually got into a released database -- no
// current code path can write one going forward.
func TestRunLibraryUpdate_LegacyRelativePathStaysMaintainable(t *testing.T) {
	bp := func(v bool) *bool { return &v }
	isolateCommandsEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state", "test.db")
	cfg := writeCommandsConfig(t, dbPath)

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO libraries (id, path, name) VALUES (1, 'music', 'Music')`,
	); err != nil {
		_ = sqlDB.Close()
		t.Fatalf("seed legacy relative-path row: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	// Rename with --path omitted must succeed: commands.go re-submits the
	// stored relative path unchanged, which must not be rejected as if the
	// operator had typed it.
	var out bytes.Buffer
	code := runLibrary(ctx, &out, LibraryCmd{Update: &LibraryUpdateCmd{
		ID:         1,
		Name:       "Music Renamed",
		ConfigPath: cfg,
	}})
	if code != 0 {
		t.Fatalf("rename of legacy relative-path row: exit code = %d, output = %q; want 0", code, out.String())
	}
	lib := getLibraryForTest(t, ctx, dbPath)
	if lib.Name != "Music Renamed" {
		t.Fatalf("Name = %q; want %q", lib.Name, "Music Renamed")
	}

	// Settings toggle with --path omitted must also succeed on the same
	// legacy row.
	out.Reset()
	code = runLibrary(ctx, &out, LibraryCmd{Update: &LibraryUpdateCmd{
		ID:         1,
		Enrich:     bp(true),
		ConfigPath: cfg,
	}})
	if code != 0 {
		t.Fatalf("settings toggle on legacy relative-path row: exit code = %d, output = %q; want 0", code, out.String())
	}
	lib = getLibraryForTest(t, ctx, dbPath)
	if lib.EnrichRecording == nil || !*lib.EnrichRecording {
		t.Fatalf("EnrichRecording = %v; want true", lib.EnrichRecording)
	}

	// An operator-supplied relative path must still be rejected on Update,
	// even for this same legacy row.
	out.Reset()
	code = runLibrary(ctx, &out, LibraryCmd{Update: &LibraryUpdateCmd{
		ID:         1,
		Path:       "still-relative",
		ConfigPath: cfg,
	}})
	if code == 0 {
		t.Fatalf("operator-supplied relative --path: exit code = 0, output = %q; want a non-zero (rejected) exit code", out.String())
	}

	// And an operator-supplied relative path must still be rejected on Add.
	out.Reset()
	code = runLibrary(ctx, &out, LibraryCmd{Add: &LibraryAddCmd{
		Path:       "also-relative",
		Name:       "Also Relative",
		ConfigPath: cfg,
	}})
	if code == 0 {
		t.Fatalf("Add with relative path: exit code = 0, output = %q; want a non-zero (rejected) exit code", out.String())
	}
}
