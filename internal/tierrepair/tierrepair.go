// Package tierrepair holds the decision logic of the two sync-tier backfills,
// `scan reconcile-sync-tier` (#1075) and `scan reconcile-remediated` (#1143):
// which action a candidate row gets from the files on disk, the guarded apply
// loop, and the write-ahead JSONL backup records. It takes a queue (an
// interface) and a backup Sink instead of a config path, never opens a
// database and never prints; the commands keep config loading, the database
// open and the aggregate stdout. A repair.Repair adapter for the web UI is to
// wrap it (#1546).
//
// Library paths, artists, titles and lyric lines never reach a count, a log
// field, a backup record or an error. The one path an error may name is the
// operator's own backup file (a failed backup write or open).
package tierrepair

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sydlexius/canticle/internal/lyrics"
)

// ErrNoBackup reports an applying run given no backup Sink. A run that
// changes rows without a backup is refused, never silently unbacked.
var ErrNoBackup = errors.New("tierrepair: an applying run needs a backup sink")

// Sink receives the write-ahead backup records. *os.File satisfies it; each
// record is Written and then Synced before the row's transaction commits.
type Sink interface {
	io.Writer
	Sync() error
}

// Progress receives done rows of total; it must not block.
type Progress func(done, total int64)

// Options control one run. A dry run (Apply false) only counts and ignores
// Backup.
type Options struct {
	Apply    bool
	Backup   Sink
	Progress Progress
}

func (o Options) check() error {
	if o.Apply && o.Backup == nil {
		return ErrNoBackup
	}
	return nil
}

func (o Options) report(done, total int) {
	if o.Progress != nil {
		o.Progress(int64(done), int64(total))
	}
}

// OpenBackup opens (creating, appending) the JSONL backup file at path with
// owner-only permissions and fsyncs its directory so the new entry survives a
// crash. The caller closes it. A bad path fails here, before any row is
// written.
func OpenBackup(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // reason: G304 -- path is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
	if err != nil {
		return nil, err //nolint:wrapcheck // reason: the caller logs the open failure with its own context
	}
	lyrics.FsyncDir(filepath.Dir(path))
	return f, nil
}

// writeRecord writes and fsyncs one JSONL line to s.
func writeRecord(s Sink, what string, b []byte) error {
	if _, err := s.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write %s backup record: %w", what, err)
	}
	if err := s.Sync(); err != nil {
		return fmt.Errorf("sync %s backup record: %w", what, err)
	}
	return nil
}
