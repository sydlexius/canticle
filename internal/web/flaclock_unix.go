//go:build linux || darwin

package web

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockFlacDir takes dir's liveness lock. The lock file is created and locked
// under a temp name, then renamed into place, so a sweeper never finds a
// .lock that exists but is not yet held.
func lockFlacDir(dir string) (*os.File, error) {
	f, err := os.CreateTemp(dir, ".lock-*")
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, flacLockName))
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// flacDirDead reports whether the cache dir's owner is gone: its lock file
// exists and can be locked. The probe lock is released when it returns; a
// sibling that loses the race to remove the dir just finds it missing. A dir
// with no lock yet may be a live sibling being created, so it is never dead.
func flacDirDead(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, flacLockName), os.O_RDONLY|unix.O_NOFOLLOW, 0) //nolint:gosec // reason: G304 -- dir is a prefix-named directory entry of the cache parent, opened read-only with O_NOFOLLOW and only flocked, never read
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil
}
