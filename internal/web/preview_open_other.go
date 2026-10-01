//go:build !unix

package web

// previewOpenFlags is empty off unix, which has no FIFO-in-the-tree hazard.
const previewOpenFlags = 0
