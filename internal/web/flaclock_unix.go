//go:build linux || darwin

package web

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

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

// flacEUID is the euid a swept dir must belong to; a test seam.
var flacEUID = os.Geteuid

// flacDirDead reports whether the cache dir's owner is gone: its lock file
// exists and can be locked. The probe lock is released when it returns; a
// sibling that loses the race to remove the dir just finds it missing. A dir
// with no lock yet may be a live sibling being created, so it is dead only once
// it is older than flacLocklessStale. A dir this euid does not own, or a .lock
// that is not a regular file (a symlink, or a FIFO, which would block the
// open), is never dead: on a shared /tmp another user can plant either.
func flacDirDead(dir string) bool {
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != flacEUID() {
		return false
	}
	fd, err := unix.Open(filepath.Join(dir, flacLockName), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return errors.Is(err, unix.ENOENT) && time.Since(fi.ModTime()) > flacLocklessStale
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return false
	}
	return unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) == nil
}
