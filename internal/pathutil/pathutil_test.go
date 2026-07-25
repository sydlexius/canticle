package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWithinRoot(t *testing.T) {
	cases := []struct {
		root, p string
		want    bool
	}{
		{"/music", "/music", true},
		{"/music", "/music/a/b.mp3", true},
		{"/music", "/musicother/x", false},
		{"/music", "/other", false},
		{"/music/sub", "/music", false},
		{"/music", "/music/../etc/passwd", false}, // cleans to /etc/passwd
		{"/music/", "/music/a", true},             // trailing slash on root
	}
	for _, c := range cases {
		if got := WithinRoot(c.root, c.p); got != c.want {
			t.Errorf("WithinRoot(%q, %q) = %v; want %v", c.root, c.p, got, c.want)
		}
	}
}

func TestEmptyInputsFailClosed(t *testing.T) {
	for _, c := range []struct{ root, p string }{
		{"", ""},
		{"", "/music/a"},
		{"/music", ""},
	} {
		if WithinRoot(c.root, c.p) {
			t.Errorf("WithinRoot(%q, %q) = true; want false (empty inputs must fail closed)", c.root, c.p)
		}
	}
	// ResolveWithinRoot delegates to WithinRoot first, so it inherits the guard.
	if _, ok := ResolveWithinRoot("", "/x"); ok {
		t.Error("ResolveWithinRoot with empty root ok = true; want false")
	}
	if _, ok := ResolveWithinRoot("/x", ""); ok {
		t.Error("ResolveWithinRoot with empty candidate ok = true; want false")
	}
}

func TestResolveWithinRootAcceptsRealFileInRoot(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "Artist")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file := filepath.Join(sub, "song.flac")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, ok := ResolveWithinRoot(root, file)
	if !ok {
		t.Fatal("ResolveWithinRoot ok = false; want true for a real file inside the root")
	}
	// The returned path is symlink-resolved (handles e.g. /var -> /private/var on
	// macOS), so compare against the resolved file path, not the raw temp path.
	want, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != want {
		t.Errorf("resolved = %q; want %q", got, want)
	}
}

func TestResolveWithinRootRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.flac")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(root, "link.flac")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	// Precondition: the symlink is lexically inside the root, so the old
	// lexical-only check would have accepted it.
	if !WithinRoot(root, link) {
		t.Fatal("precondition failed: symlink should be lexically within the root")
	}
	// Symlink resolution must reject it because it points outside the root.
	if got, ok := ResolveWithinRoot(root, link); ok {
		t.Errorf("ResolveWithinRoot returned %q, ok=true for a symlink escaping the root; want rejected", got)
	}
}

func TestResolveWithinRootRejectsNonexistent(t *testing.T) {
	root := t.TempDir()
	if _, ok := ResolveWithinRoot(root, filepath.Join(root, "missing.flac")); ok {
		t.Error("ResolveWithinRoot ok = true for a nonexistent path; want rejected (fail closed)")
	}
}

func TestResolveWithinRootRejectsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	file := filepath.Join(outside, "song.flac")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, ok := ResolveWithinRoot(root, file); ok {
		t.Error("ResolveWithinRoot ok = true for a path outside the root; want rejected")
	}
}

func TestCanonicalRootAndRebaseCollapseSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(realRoot, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	symlinkRoot := filepath.Join(base, "link")
	if err := os.Symlink(realRoot, symlinkRoot); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	file := filepath.Join(realRoot, "sub", "song.flac")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	wantCanon, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	// Walking through the symlinked root, as scanDir would (dir + file.Name()
	// joins, never resolving the root itself per file).
	walked := filepath.Join(symlinkRoot, "sub", "song.flac")

	absRoot, canonRoot := CanonicalRoot(symlinkRoot)
	got := RebaseUnderCanonicalRoot(absRoot, canonRoot, walked)
	if got != wantCanon {
		t.Errorf("RebaseUnderCanonicalRoot(%q, %q, %q) = %q; want %q", absRoot, canonRoot, walked, got, wantCanon)
	}

	// A path outside absRoot falls back to a direct resolve rather than
	// producing a nonsensical rebased value.
	otherFile := filepath.Join(base, "other.flac")
	if err := os.WriteFile(otherFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("write other file: %v", err)
	}
	wantOther, err := filepath.EvalSymlinks(otherFile)
	if err != nil {
		t.Fatalf("EvalSymlinks other: %v", err)
	}
	if got := RebaseUnderCanonicalRoot(absRoot, canonRoot, otherFile); got != wantOther {
		t.Errorf("RebaseUnderCanonicalRoot for an out-of-root path = %q; want fallback %q", got, wantOther)
	}
}

func TestCanonicalPathResolvesSymlinkedComponent(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkDir := filepath.Join(base, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	file := filepath.Join(realDir, "song.flac")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	want, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got := CanonicalPath(filepath.Join(linkDir, "song.flac")); got != want {
		t.Errorf("CanonicalPath = %q; want %q", got, want)
	}
}

func TestCanonicalRootDegradesOnNonexistentRoot(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "does-not-exist")
	absRoot, canonRoot := CanonicalRoot(missing)
	wantAbs, err := filepath.Abs(missing)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if absRoot != wantAbs {
		t.Errorf("absRoot = %q; want %q", absRoot, wantAbs)
	}
	// EvalSymlinks fails on a root that does not exist yet (e.g. a scan
	// starting before the configured mount materializes); canonRoot must
	// degrade to the absolute form rather than erroring out the caller.
	if canonRoot != wantAbs {
		t.Errorf("canonRoot = %q; want degraded fallback %q", canonRoot, wantAbs)
	}
}

func TestCanonicalPathDegradesOnNonexistent(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "does-not-exist.flac")
	got := CanonicalPath(missing)
	want, err := filepath.Abs(missing)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if got != want {
		t.Errorf("CanonicalPath for a nonexistent path = %q; want the absolute-but-unresolved fallback %q", got, want)
	}
}
