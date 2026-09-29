//go:build !unix

package lyrics

import "os"

// openNoFollow opens path read-only. This platform has no O_NOFOLLOW, so the
// caller's fstat-plus-SameFile check on the handle is what refuses a swap.
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // reason: path is a sidecar of the writer's own target; the handle is fstat'ed regular and SameFile-checked before any read
}
