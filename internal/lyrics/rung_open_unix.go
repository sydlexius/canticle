//go:build unix

package lyrics

import (
	"os"
	"syscall"
)

// openNoFollow opens path read-only without following a final symlink
// (ELOOP) and without blocking on a FIFO, for readRegularNoFollow.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // reason: path is a sidecar of the writer's own target; the handle is fstat'ed regular before any read
}
