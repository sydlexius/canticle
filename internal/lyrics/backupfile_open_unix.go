//go:build unix

package lyrics

import (
	"os"
	"syscall"
)

// openBackupAppend opens a JSONL backup file for append (0600), creating it, and
// refuses a final symlink atomically (ELOOP), so a path swapped for a symlink
// after validation cannot redirect the write into another file.
func openBackupAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // reason: G304 -- path is operator-supplied (--backup) or derived from the configured db dir; the final symlink is refused by O_NOFOLLOW and the handle is judged regular by the caller
}
