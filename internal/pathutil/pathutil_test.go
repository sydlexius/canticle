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
