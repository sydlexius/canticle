package prune

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLibraryRootsAndRootOnline(t *testing.T) {
	ctx, sqlDB, _, root := openSeeded(t)
	p := New(sqlDB)
	got, err := p.LibraryRoots(ctx)
	if err != nil || len(got) != 1 || got[0] != root {
		t.Fatalf("LibraryRoots = %q err=%v, want [%q]", got, err, root)
	}
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !p.RootOnline(root) {
		t.Error("populated root must be online")
	}
	if p.RootOnline(filepath.Join(t.TempDir(), "empty")) {
		t.Error("a missing root must be offline")
	}
	// An empty mountpoint is offline, matching prune's own rule (dirPopulated).
	if p.RootOnline(t.TempDir()) {
		t.Error("an empty directory must read as offline")
	}
}
