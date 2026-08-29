package realign

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// crossDeviceDir returns a directory on a DIFFERENT filesystem from dir, or
// skips. /dev/shm is tmpfs on Linux (so CI exercises the real EXDEV path); macOS
// has no portable equivalent, so the dev box skips and CI carries this test.
func crossDeviceDir(t *testing.T, dir string) string {
	t.Helper()
	var here, there syscall.Stat_t
	if err := syscall.Stat(dir, &here); err != nil {
		t.Skipf("cannot stat %q: %v", dir, err)
	}
	const shm = "/dev/shm"
	if err := syscall.Stat(shm, &there); err != nil {
		t.Skipf("no second filesystem available (%s: %v); EXDEV dispatch is covered on Linux CI", shm, err)
	}
	if here.Dev == there.Dev {
		t.Skip("temp dir and /dev/shm are the same filesystem, so EXDEV cannot be provoked")
	}
	out, err := os.MkdirTemp(shm, "canticle-exdev-")
	if err != nil {
		t.Skipf("cannot create a temp dir on %s: %v", shm, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(out) })
	return out
}

// TestRenameOrCopyCrossesFilesystems is the regression test for #810, and it
// exercises the REAL syscall boundary rather than a simulation.
//
// Every remediation routes through renameOrCopy, and the quarantine root is
// derived from the DATABASE directory while sidecars live under a LIBRARY root.
// On the standard container layout those are different volumes, so a bare
// os.Rename returns EXDEV and nothing can ever be remediated. Measured in
// production on v1.35.0: 30 of 30 actions failed, all EXDEV.
func TestRenameOrCopyCrossesFilesystems(t *testing.T) {
	src := t.TempDir()
	dst := crossDeviceDir(t, src)

	orphan := filepath.Join(src, "track.lrc")
	const body = "[00:10.00]alpha\n[02:30.00]beta\n"
	if err := os.WriteFile(orphan, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	target := filepath.Join(dst, "track.lrc")

	// Prove the premise: a bare rename MUST fail here, or the test is vacuous.
	if err := os.Rename(orphan, target); !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("os.Rename returned %v, want EXDEV; the fixture is not cross-device so this test proves nothing", err)
	}

	if err := renameOrCopy(orphan, target); err != nil {
		t.Fatalf("renameOrCopy across filesystems: %v", err)
	}
	got, err := os.ReadFile(target) //nolint:gosec // reason: G304: test-controlled path
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != body {
		t.Errorf("target content = %q, want %q", got, body)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Error("the source still exists; a move must not leave the original behind")
	}
}

// TestCopyFileDurableRefusesAnExistingDestination pins the clobber-safety the
// rename had. The caller checks first, but the check and the write are not
// atomic, so O_EXCL has to refuse a file that appeared in between.
func TestCopyFileDurableRefusesAnExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.lrc")
	dst := filepath.Join(dir, "dst.lrc")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("PRECIOUS"), 0o600); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	if err := copyFileDurable(src, dst); err == nil {
		t.Error("overwrote an existing destination; a remediation must never clobber a sidecar")
	}
	got, err := os.ReadFile(dst) //nolint:gosec // reason: G304: test-controlled path
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "PRECIOUS" {
		t.Errorf("destination content = %q, want it untouched", got)
	}
}

// TestCopyFileDurableLeavesNoPartialOnFailure pins the cleanup contract: a copy
// that fails AFTER creating its destination must remove that partial file and
// leave the SOURCE intact, so the caller's "leave the file in place" promise
// holds.
//
// The source is a DIRECTORY, which is what makes this reach the cleanup path:
// os.Open and Stat both succeed on a directory, so the destination IS created,
// and the failure lands in io.Copy (EISDIR) with a partial file already on disk.
// An earlier version of this test pointed the destination under a non-directory,
// which failed at OpenFile BEFORE anything was created -- it passed without ever
// executing the cleanup it claimed to cover, and its own "no partial survived"
// assertion was wrong anyway, since os.IsNotExist is false for ENOTDIR.
func TestCopyFileDurableLeavesNoPartialOnFailure(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src-is-a-directory")
	if err := os.Mkdir(src, 0o750); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	dst := filepath.Join(dir, "dst.lrc")

	if err := copyFileDurable(src, dst); err == nil {
		t.Fatal("expected a failure copying from a directory")
	}
	if _, err := os.Lstat(src); err != nil {
		t.Errorf("the source was disturbed by a failed copy: %v", err)
	}
	if _, err := os.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a partial destination survived a failed copy (Lstat err = %v)", err)
	}
}

// TestRenameOrCopyUsesRenameOnOneFilesystem keeps the common path atomic: the
// fallback exists for the cross-device case and must not displace the rename.
func TestRenameOrCopyUsesRenameOnOneFilesystem(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.lrc")
	dst := filepath.Join(dir, "moved.lrc")
	if err := os.WriteFile(src, []byte("body"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := renameOrCopy(src, dst); err != nil {
		t.Fatalf("renameOrCopy: %v", err)
	}
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Error("source survived a same-filesystem move")
	}
	got, err := os.ReadFile(dst) //nolint:gosec // reason: G304: test-controlled path
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "body" {
		t.Errorf("content = %q, want %q", got, "body")
	}
}

// forceEXDEV makes renameFile report a cross-device link for the duration of a
// test, so the fallback is reachable without two real mounts.
func forceEXDEV(t *testing.T) {
	t.Helper()
	prev := renameFile
	renameFile = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameFile = prev })
}

// TestRenameOrCopyFallsBackOnEXDEV exercises the whole fallback in EVERY
// environment, not just where a second filesystem happens to exist.
//
// The real cross-device test above is the end-to-end proof, but it can only run
// on Linux. Leaving the fallback covered by that alone would mean the code this
// fix CONSISTS OF goes unexercised on the dev box -- and running unexercised on
// the machine where the change is written is precisely how #810 shipped.
func TestRenameOrCopyFallsBackOnEXDEV(t *testing.T) {
	forceEXDEV(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "track.lrc")
	dst := filepath.Join(dir, "quarantine", "track.lrc")
	const body = "[00:10.00]alpha\n[02:30.00]beta\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := renameOrCopy(src, dst); err != nil {
		t.Fatalf("renameOrCopy on EXDEV: %v", err)
	}
	got, err := os.ReadFile(dst) //nolint:gosec // reason: G304: test-controlled path
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != body {
		t.Errorf("target content = %q, want %q", got, body)
	}
	if _, err := os.Lstat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the source survived; a move must not leave the original behind")
	}
}

// TestRenameOrCopyReportsANonEXDEVFailure keeps the fallback NARROW. Only a
// cross-device link may take the copy path: any other rename failure is the real
// error and must surface as itself, not be masked by a copy that might succeed
// for the wrong reason.
func TestRenameOrCopyReportsANonEXDEVFailure(t *testing.T) {
	prev := renameFile
	sentinel := errors.New("disk is on fire")
	renameFile = func(string, string) error { return sentinel }
	t.Cleanup(func() { renameFile = prev })

	dir := t.TempDir()
	src := filepath.Join(dir, "track.lrc")
	if err := os.WriteFile(src, []byte("body"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dst := filepath.Join(dir, "moved.lrc")

	err := renameOrCopy(src, dst)
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the original rename failure; a non-EXDEV error must not be masked by the copy path", err)
	}
	if _, serr := os.Lstat(dst); !errors.Is(serr, fs.ErrNotExist) {
		t.Error("a destination was created for a failure that should not have copied at all")
	}
}

// TestRenameOrCopyKeepsBothCopiesWhenTheUnlinkFails pins the crash-ordering
// choice. The source is removed only after the destination is durable, so a
// failure at that last step leaves BOTH files -- recoverable by hand. The
// reverse order would lose the file outright, which is why it is not used.
func TestRenameOrCopyKeepsBothCopiesWhenTheUnlinkFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a root process can unlink from a read-only directory")
	}
	forceEXDEV(t)
	base := t.TempDir()
	srcDir := filepath.Join(base, "library")
	if err := os.Mkdir(srcDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := filepath.Join(srcDir, "track.lrc")
	if err := os.WriteFile(src, []byte("body"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dst := filepath.Join(base, "track.lrc")
	// Deny unlink by making the SOURCE directory read-only.
	if err := os.Chmod(srcDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(srcDir, 0o750) })

	err := renameOrCopy(src, dst)
	if err == nil {
		t.Fatal("expected an error when the original could not be removed")
	}
	// BOTH files must survive: the copy is durable, the original is still there.
	if _, serr := os.Lstat(dst); serr != nil {
		t.Errorf("the durable copy was discarded on an unlink failure: %v", serr)
	}
	if _, serr := os.Lstat(src); serr != nil {
		t.Errorf("the original vanished despite the unlink failing: %v", serr)
	}
}

// TestRenameOrCopyReportsACopyFailure covers the error path OUT of the fallback:
// when the copy itself fails, renameOrCopy must surface that and leave the
// source exactly where it was, because the caller's contract on any failure is
// "the file is still in place".
func TestRenameOrCopyReportsACopyFailure(t *testing.T) {
	forceEXDEV(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "track.lrc")
	dst := filepath.Join(dir, "taken.lrc")
	if err := os.WriteFile(src, []byte("body"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	// A destination that already exists: O_EXCL refuses it, so the copy fails.
	if err := os.WriteFile(dst, []byte("PRECIOUS"), 0o600); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	if err := renameOrCopy(src, dst); err == nil {
		t.Fatal("expected an error when the copy could not be made")
	}
	if _, err := os.Lstat(src); err != nil {
		t.Errorf("the source was disturbed by a failed copy: %v", err)
	}
	got, err := os.ReadFile(dst) //nolint:gosec // reason: G304: test-controlled path
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "PRECIOUS" {
		t.Errorf("destination content = %q, want it untouched", got)
	}
}

// TestCopyFileDurableReportsAnUnreadableSource covers the open failure. It is
// the ordinary way a copy fails in production -- the file vanished, or is not
// readable -- and it must report rather than create anything.
func TestCopyFileDurableReportsAnUnreadableSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "gone.lrc")
	dst := filepath.Join(dir, "dst.lrc")

	if err := copyFileDurable(src, dst); err == nil {
		t.Fatal("expected an error copying a file that does not exist")
	}
	if _, err := os.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a destination was created for a source that could not be opened")
	}
}
