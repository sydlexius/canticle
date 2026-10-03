//go:build linux || darwin

package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// sweepWithin runs a new cache's startup sweep under parent and fails if it
// does not return within 5s.
func sweepWithin(t *testing.T, parent string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		c, err := newFlacCache(parent, 1<<20, nil)
		if err == nil {
			err = c.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("newFlacCache blocked: the sweep's lock probe hangs")
	}
}

// plantDir makes a prefix-named sibling dir holding a data file, with .lock
// created by mk (nil: no .lock).
func plantDir(t *testing.T, parent, name string, mk func(lock string) error) string {
	t.Helper()
	d := filepath.Join(parent, flacDirPrefix+name)
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "x.flac"), fakeFlac, 0o600); err != nil {
		t.Fatal(err)
	}
	if mk != nil {
		if err := mk(filepath.Join(d, flacLockName)); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func requirePresent(t *testing.T, d string, want bool) {
	t.Helper()
	if _, err := os.Lstat(d); (err == nil) != want {
		t.Fatalf("%s present = %v, want %v", filepath.Base(d), err == nil, want)
	}
}

// A FIFO planted as .lock (any local user can on a shared /tmp) must neither
// hang startup nor read as dead.
func TestFlacSweepFifoLockNeitherHangsNorDeletes(t *testing.T) {
	parent := t.TempDir()
	d := plantDir(t, parent, "fifo", func(l string) error { return unix.Mkfifo(l, 0o666) })
	sweepWithin(t, parent)
	requirePresent(t, d, true)
}

// A .lock that is not a regular file (here a directory, which opens and
// flocks fine) is not this package's lock, so the dir reads as live.
func TestFlacSweepNonRegularLockKeepsDir(t *testing.T) {
	parent := t.TempDir()
	d := plantDir(t, parent, "dirlock", func(l string) error { return os.Mkdir(l, 0o700) })
	sweepWithin(t, parent)
	requirePresent(t, d, true)
}

// A .lock that is a symlink is never followed and the dir reads as live,
// even when the link's target is a free regular file.
func TestFlacSweepSymlinkLockKeepsDir(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(t.TempDir(), "free")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d := plantDir(t, parent, "link", func(l string) error { return os.Symlink(target, l) })
	sweepWithin(t, parent)
	requirePresent(t, d, true)
}

// A dir this euid does not own is never probed or removed, even with a free
// lock: another user may have planted it, or it is theirs.
func TestFlacSweepSkipsForeignDir(t *testing.T) {
	parent := t.TempDir()
	d := plantDir(t, parent, "foreign", func(l string) error { return os.WriteFile(l, nil, 0o600) })
	old := flacEUID
	flacEUID = func() int { return os.Geteuid() + 1 }
	t.Cleanup(func() { flacEUID = old })
	sweepWithin(t, parent)
	requirePresent(t, d, true)
}

// A dir with no .lock is one being created, so it is kept, unless it is over
// flacLocklessStale old: a crash before the lock rename, or a Close that
// removed .lock and then failed, left it.
func TestFlacSweepReclaimsStaleLocklessDir(t *testing.T) {
	parent := t.TempDir()
	fresh := plantDir(t, parent, "fresh", nil)
	stale := plantDir(t, parent, "stale", nil)
	old := time.Now().Add(-flacLocklessStale - time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	sweepWithin(t, parent)
	requirePresent(t, fresh, true)
	requirePresent(t, stale, false)
}

// Close releases the lock: a probe of the same lock file then succeeds.
func TestFlacCacheCloseReleasesLock(t *testing.T) {
	c, err := newFlacCache(t.TempDir(), 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Keep a handle on the lock file so the probe can reach it after
	// Close's RemoveAll unlinks the name.
	probe, err := os.Open(filepath.Join(c.dir, flacLockName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close() }()
	if unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil {
		t.Fatal("lock not held while the cache is open")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("lock still held after Close: %v", err)
	}
}

// The sweep never probes its own dir. A free regular .lock makes the dir read
// as dead here, as the cache's own dir would where flock is per-process (NFS)
// or a no-op (some FUSE filesystems).
func TestFlacSweepSkipsOwnDir(t *testing.T) {
	parent := t.TempDir()
	own := plantDir(t, parent, "own", func(l string) error { return os.WriteFile(l, nil, 0o600) })
	if !flacDirDead(own) {
		t.Fatal("setup: a free .lock should read as dead")
	}
	sweepFlacDirs(parent, own)
	requirePresent(t, own, true)
}
