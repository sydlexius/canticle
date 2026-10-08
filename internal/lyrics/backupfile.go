package lyrics

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// DefaultBackupPath is where a mark action's JSONL backup goes when the operator
// names no file: <dir of dbPath>/<stem>-backup-<UTC timestamp>.jsonl. The CLI
// and the web UI both use it, so a backup lands in the same folder under the
// same names whichever surface wrote it.
func DefaultBackupPath(dbPath, stem string, now time.Time) string {
	return filepath.Join(filepath.Dir(dbPath), fmt.Sprintf("%s-backup-%s.jsonl", stem, now.UTC().Format("20060102-150405")))
}

// LazyBackupFile opens a JSONL backup file (0600, append) on first use, so a run
// that replaces nothing leaves no empty file behind. The open refuses a final
// symlink; errors carry no path.
type LazyBackupFile struct {
	Path, What string
	f          *os.File
}

// File returns the open backup file, opening it on the first call.
func (b *LazyBackupFile) File() (*os.File, error) {
	if b.f != nil {
		return b.f, nil
	}
	f, err := openBackup(b.Path)
	if err != nil {
		// The PathError text carries the path; surface only the cause.
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return nil, fmt.Errorf("open %s: %w", b.What, err)
	}
	if err := checkBackupHandle(f, b.Path); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("open %s: %w", b.What, err)
	}
	// O_CREATE's mode only applies to a new file; tighten an existing one before any write.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("tighten %s mode: %w", b.What, err)
	}
	b.f = f
	FsyncDir(filepath.Dir(b.Path))
	return f, nil
}

// Opened reports whether a backup file was opened (something was reported).
func (b *LazyBackupFile) Opened() bool { return b.f != nil }

// Close closes the file if it was opened.
func (b *LazyBackupFile) Close() {
	if b.f != nil {
		if err := b.f.Close(); err != nil {
			slog.Warn("failed to close backup file", "what", b.What, "error", err)
		}
	}
}

// openBackup is the open seam; tests swap in a follow-capable open to prove the
// shared check below does not depend on O_NOFOLLOW.
var openBackup = openBackupAppend

var errBackupNotRegular = errors.New("not a regular file")

// checkBackupHandle runs on every platform after the open: the path must Lstat
// as a regular file (not a symlink) and be the very file the handle refers to.
// The error carries no path.
func checkBackupHandle(f *os.File, path string) error {
	hfi, err := f.Stat()
	if err != nil || !hfi.Mode().IsRegular() {
		return errBackupNotRegular
	}
	lfi, err := os.Lstat(path)
	if err != nil || !lfi.Mode().IsRegular() || !os.SameFile(hfi, lfi) {
		return errBackupNotRegular
	}
	return nil
}
