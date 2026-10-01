//go:build unix

package web

import (
	"os"
	"syscall"
)

// openPreviewAudio opens a confined, symlink-resolved audio path read-only
// without following a final symlink (ELOOP) and without blocking on a FIFO.
func openPreviewAudio(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // reason: path was confined by pathutil.ResolveWithinRoot; the handle is fstat'ed regular and SameFile-checked before any byte is served
}
