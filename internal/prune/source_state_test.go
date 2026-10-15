package prune

import (
	"os"
	"path/filepath"
	"testing"
)

// SourceStateOf is the worker's gone authority (#1597). Each case changes one
// condition of the proof; every doubt must answer SourceUnproven.
func TestSourceStateOf(t *testing.T) {
	mk := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, root string) string // returns the source path asked about
		want  SourceState
	}{
		{"present", func(t *testing.T, root string) string {
			p := filepath.Join(root, "amb", "New", "01.flac")
			mk(t, p)
			return p
		}, SourcePresent},
		{"whole-folder-gone-root-populated", func(t *testing.T, root string) string {
			mk(t, filepath.Join(root, "keep", "x.flac"))
			return filepath.Join(root, "amb", "Gone", "01.flac")
		}, SourceGone},
		{"file-gone-folder-populated", func(t *testing.T, root string) string {
			mk(t, filepath.Join(root, "amb", "New", "02.flac"))
			return filepath.Join(root, "amb", "New", "01.flac")
		}, SourceGone},
		{"root-empty-is-offline", func(t *testing.T, root string) string {
			return filepath.Join(root, "amb", "Gone", "01.flac")
		}, SourceUnproven},
		{"sub-mountpoint-empty-in-live-root", func(t *testing.T, root string) string {
			mk(t, filepath.Join(root, "keep", "x.flac"))
			if err := os.MkdirAll(filepath.Join(root, "amb"), 0o755); err != nil { // an unmounted share reads empty
				t.Fatal(err)
			}
			return filepath.Join(root, "amb", "Gone", "01.flac")
		}, SourceUnproven},
		{"outside-every-root", func(t *testing.T, root string) string {
			mk(t, filepath.Join(root, "keep", "x.flac"))
			return filepath.Join(t.TempDir(), "amb", "01.flac")
		}, SourceUnproven},
		{"symlink-then-dotdot-escapes-the-root", func(t *testing.T, root string) string {
			// root/escape -> an outside dir; root/escape/../01.flac cleans to
			// root/01.flac (inside, absent) while lstat reads outside/../01.flac.
			mk(t, filepath.Join(root, "keep", "x.flac"))
			outside := filepath.Join(t.TempDir(), "deep")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
				t.Fatal(err)
			}
			return root + "/escape/../01.flac"
		}, SourceUnproven},
		{"empty-path", func(t *testing.T, root string) string {
			mk(t, filepath.Join(root, "keep", "x.flac"))
			return ""
		}, SourceUnproven},
		{"stat-error-other-than-not-exist", func(t *testing.T, root string) string {
			// The folder is readable (so populated) but not searchable: lstat of
			// the file inside fails with EACCES, which is not "gone".
			p := filepath.Join(root, "amb", "New", "01.flac")
			mk(t, p)
			dir := filepath.Dir(p)
			if err := os.Chmod(dir, 0o444); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			if _, err := os.Lstat(p); err == nil {
				t.Skip("directory permissions are not enforced here (running as root?)")
			}
			return p
		}, SourceUnproven},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sqlDB, _, root := openSeeded(t)
			src := tc.setup(t, root)
			got, err := New(sqlDB).SourceStateOf(ctx, src)
			if err != nil || got != tc.want {
				t.Errorf("SourceStateOf = %v err=%v, want %v", got, err, tc.want)
			}
		})
	}
}

// A library root symlink whose target went away does not resolve: nothing under
// it is proven gone, however the cached online answer reads (#1597).
func TestSourceStateOf_UnresolvedSymlinkRootIsUnproven(t *testing.T) {
	ctx, sqlDB, _, link, real := symlinkedLibrary(t)
	src := filepath.Join(link, "amb", "Gone", "01.flac")
	if got, err := New(sqlDB).SourceStateOf(ctx, src); err != nil || got != SourceGone {
		t.Fatalf("control: SourceStateOf with the symlink resolving = %v err=%v, want SourceGone", got, err)
	}
	if err := os.RemoveAll(real); err != nil { // the mount drops
		t.Fatal(err)
	}
	if got, err := New(sqlDB).SourceStateOf(ctx, src); err != nil || got != SourceUnproven {
		t.Errorf("SourceStateOf under an unresolved root = %v err=%v, want SourceUnproven", got, err)
	}
}
