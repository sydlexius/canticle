//go:build unix

package reports_test

import "syscall"

// mkfifo creates a named pipe for the opens-nothing preview test.
func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
