//go:build unix

package lyrics

import "syscall"

// mkfifo creates a named pipe for the special-file companion test.
func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
