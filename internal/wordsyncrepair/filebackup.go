package wordsyncrepair

import (
	"errors"
	"fmt"
	"os"

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/tierrepair"
)

// errNilBackup is returned by a method called on a nil *FileBackup.
var errNilBackup = errors.New("reconcile-word-sync backup: nil FileBackup")

// FileBackup is a Backup over a JSONL file that is created on the first record,
// so a run that flips nothing leaves no file. A nil *FileBackup is refused with
// an error rather than a panic.
type FileBackup struct {
	Path string
	// Write and SyncFile are test seams: production callers leave them nil, and
	// when set they replace the default record write and fsync.
	Write    func(f *os.File, p queue.WordRecheckPrior) error
	SyncFile func(f *os.File) error

	f *os.File
}

// Record opens the file on first use and writes p.
func (b *FileBackup) Record(p queue.WordRecheckPrior) error {
	if b == nil {
		return errNilBackup
	}
	if b.f == nil {
		f, err := tierrepair.OpenBackup(b.Path)
		if err != nil {
			return fmt.Errorf("open reconcile-word-sync backup %q: %w", b.Path, err)
		}
		b.f = f
	}
	if b.Write != nil {
		return b.Write(b.f, p)
	}
	return AppendRecord(b.f, p)
}

// Sync fsyncs the file; it is a no-op before the first record.
func (b *FileBackup) Sync() error {
	if b == nil {
		return errNilBackup
	}
	if b.f == nil {
		return nil
	}
	if b.SyncFile != nil {
		return b.SyncFile(b.f)
	}
	return b.f.Sync() //nolint:wrapcheck // reason: the caller wraps the batch failure
}

// Opened reports whether a record was ever attempted (the file exists).
func (b *FileBackup) Opened() bool { return b != nil && b.f != nil }

// Close closes the file if it was opened.
func (b *FileBackup) Close() error {
	if b == nil || b.f == nil {
		return nil
	}
	return b.f.Close() //nolint:wrapcheck // reason: the caller logs the close failure with its own context
}
