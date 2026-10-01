//go:build !unix

package web

import "os"

// openPreviewAudio opens a confined audio path read-only. This platform has no
// O_NOFOLLOW, so the caller's fstat-plus-SameFile check on the handle is what
// refuses a swap.
func openPreviewAudio(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // reason: path was confined by pathutil.ResolveWithinRoot; the handle is fstat'ed regular and SameFile-checked before any byte is served
}
