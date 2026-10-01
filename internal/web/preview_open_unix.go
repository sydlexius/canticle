//go:build unix

package web

import "syscall"

// previewOpenFlags keeps opening a FIFO planted at an audio path from blocking
// the request goroutine; the regular-file fstat then refuses it.
const previewOpenFlags = syscall.O_NONBLOCK
