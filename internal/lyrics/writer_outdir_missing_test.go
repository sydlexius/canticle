package lyrics

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveOutdir_MissingOnlyWhenNoSymlinkInvolved pins #1430: the
// ErrOutputDirMissing sentinel is acted on by the worker, so it must mean a
// genuinely absent directory. A dangling symlink (even one pointing out of the
// root) is not "missing" and must keep the confinement refusal.
func TestResolveOutdir_MissingOnlyWhenNoSymlinkInvolved(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(root, "real")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	mustLink := func(target, link string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	mustLink(filepath.Join(outside, "gone"), filepath.Join(root, "escape"))
	mustLink(filepath.Join(root, "nowhere"), filepath.Join(root, "inroot"))

	cases := []struct {
		name        string
		outdir      string
		wantMissing bool
	}{
		{"escaping dangling symlink at leaf", filepath.Join(root, "escape"), false},
		{"escaping dangling symlink at parent", filepath.Join(root, "escape", "sub"), false},
		{"in-root dangling symlink at leaf", filepath.Join(root, "inroot"), false},
		{"in-root dangling symlink at parent", filepath.Join(root, "inroot", "sub"), false},
		{"plain missing dir", filepath.Join(root, "absent"), true},
		{"missing dir under real parent", filepath.Join(root, "real", "absent", "deeper"), true},
	}
	w := NewLRCWriter(root)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.resolveOutdir(tc.outdir)
			if err == nil {
				t.Fatal("resolveOutdir: want an error, got nil")
			}
			if got := errors.Is(err, ErrOutputDirMissing); got != tc.wantMissing {
				t.Fatalf("errors.Is(err, ErrOutputDirMissing) = %v, want %v (err: %v)", got, tc.wantMissing, err)
			}
		})
	}
}
