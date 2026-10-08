//go:build !unix

package lyrics

import "os"

// openBackupAppend opens a JSONL backup file for append (0600), creating it.
// This platform has no O_NOFOLLOW, so the caller's handle check (regular file)
// is the refusal.
func openBackupAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // reason: G304 -- path is operator-supplied (--backup) or derived from the configured db dir; the handle is judged regular by the caller
}
