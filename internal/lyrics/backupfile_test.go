package lyrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDefaultBackupPath(t *testing.T) {
	// A non-UTC timestamp proves the name is rendered in UTC.
	loc := time.FixedZone("PDT", -7*3600)
	now := time.Date(2026, 10, 7, 21, 28, 5, 0, loc)
	got := DefaultBackupPath(filepath.Join("data", "dir", "canticle.db"), "mark-wrong", now)
	want := filepath.Join("data", "dir", "mark-wrong-backup-20261008-042805.jsonl")
	if got != want {
		t.Errorf("DefaultBackupPath = %q, want %q", got, want)
	}
}

func TestLazyBackupFileCreatesOnFirstUseAndAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.jsonl")
	b := &LazyBackupFile{Path: path, What: "backup"}

	// A dry run (nothing reported) leaves no file and Close is a no-op.
	if b.Opened() {
		t.Fatal("Opened() true before first use")
	}
	b.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file exists before first record: %v", err)
	}

	f, err := b.File()
	if err != nil {
		t.Fatal(err)
	}
	if !b.Opened() {
		t.Error("Opened() false after File()")
	}
	if f2, err := b.File(); err != nil || f2 != f {
		t.Errorf("second File() should return the same handle: %v", err)
	}
	for _, line := range []string{"{\"a\":1}\n", "{\"a\":2}\n"} {
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	b.Close()

	got, err := os.ReadFile(path) //nolint:gosec // reason: test reads its own temp file
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{\"a\":1}\n{\"a\":2}\n" {
		t.Errorf("content = %q", got)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode/err = %v / %v, want 0600", fi, err)
	}

	// A second run appends to, and tightens, the existing file.
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // reason: test loosens the mode to prove tightening
		t.Fatal(err)
	}
	b2 := &LazyBackupFile{Path: path, What: "backup"}
	f3, err := b2.File()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f3.WriteString("{\"a\":3}\n"); err != nil {
		t.Fatal(err)
	}
	b2.Close()
	got, _ = os.ReadFile(path) //nolint:gosec // reason: test reads its own temp file
	if string(got) != "{\"a\":1}\n{\"a\":2}\n{\"a\":3}\n" {
		t.Errorf("append content = %q", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("existing file mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestLazyBackupFileRefusesSymlinkNoFollow: with the real platform open, a
// symlinked backup path is refused and the target is untouched.
func TestLazyBackupFileRefusesSymlinkNoFollow(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.dat")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "b.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	b := &LazyBackupFile{Path: link, What: "backup"}
	if _, err := b.File(); err == nil {
		b.Close()
		t.Fatal("File() accepted a symlinked backup path")
	} else if strings.Contains(err.Error(), dir) {
		t.Errorf("error leaks the path: %s", err)
	}
	if b.Opened() {
		t.Error("a handle was retained on refusal")
	}
	if got, _ := os.ReadFile(target); string(got) != "untouched" { //nolint:gosec // reason: test reads its own temp file
		t.Errorf("target changed: %q", got)
	}
}

// TestLazyBackupFileOpenErrorCarriesNoPath: a missing parent directory fails the
// open, and the wrapped error names the operation but not the path.
func TestLazyBackupFileOpenErrorCarriesNoPath(t *testing.T) {
	dir := t.TempDir()
	b := &LazyBackupFile{Path: filepath.Join(dir, "missing-subdir", "b.jsonl"), What: "backup"}
	_, err := b.File()
	if err == nil {
		b.Close()
		t.Fatal("File() succeeded with a missing parent directory")
	}
	if !strings.Contains(err.Error(), "open backup") {
		t.Errorf("error lacks the operation: %s", err)
	}
	if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "missing-subdir") {
		t.Errorf("error leaks the path: %s", err)
	}
}

// TestLazyBackupFileRefusesSymlinkWithFollowingOpen: the post-open check refuses
// a symlinked backup path even where the open itself follows links (the
// non-unix behavior), the target is unchanged, and the error carries no path.
func TestLazyBackupFileRefusesSymlinkWithFollowingOpen(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.dat")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "b.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	orig := openBackup
	openBackup = func(p string) (*os.File, error) {
		return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // reason: test open that follows links
	}
	t.Cleanup(func() { openBackup = orig })

	b := &LazyBackupFile{Path: link, What: "backup"}
	f, err := b.File()
	if err == nil {
		b.Close()
		t.Fatal("file() accepted a symlinked backup path")
	}
	if f != nil || b.f != nil {
		t.Error("a handle was retained on refusal")
	}
	if got, rerr := os.ReadFile(target); rerr != nil || string(got) != "untouched" {
		t.Errorf("target changed: %v %q", rerr, got)
	}
	if msg := err.Error(); strings.Contains(msg, dir) {
		t.Errorf("error leaks the path: %s", msg)
	}
}

// TestPathlessStripsPathKeepsCause: the helper drops the path from a
// *os.PathError, keeps the errno reachable, and leaves nil and other errors alone.
func TestPathlessStripsPathKeepsCause(t *testing.T) {
	const secret = "/library/Private Artist/secret.jsonl"
	pe := &os.PathError{Op: "chmod", Path: secret, Err: syscall.EACCES}
	got := pathless(fmt.Errorf("wrapped: %w", pe))
	if strings.Contains(got.Error(), secret) || strings.Contains(got.Error(), "Private") {
		t.Errorf("result leaks the path: %s", got)
	}
	if !errors.Is(got, syscall.EACCES) {
		t.Errorf("errno lost: %v", got)
	}
	if pathless(nil) != nil {
		t.Error("nil did not stay nil")
	}
	plain := errors.New("plain")
	if pathless(plain) != plain {
		t.Error("non-path error not passed through unchanged")
	}
}
