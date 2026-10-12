package commands

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
)

// adoptSettledBackupName is the append-only JSONL backup of every adoption,
// next to the database.
const adoptSettledBackupName = "adopt-settled-backup.jsonl"

// adoptBackup appends one fsynced JSONL record per adoption to a
// lyrics.LazyBackupFile, opened on the first record so a pass that adopts
// nothing leaves no file. The mutex keeps a write and sync atomic.
type adoptBackup struct {
	mu sync.Mutex
	f  lyrics.LazyBackupFile
}

func (b *adoptBackup) append(rec queue.AdoptRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal adopt backup record: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := b.f.File()
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write adopt backup record: %w", pathlessErr(err))
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync adopt backup record: %w", pathlessErr(err))
	}
	return nil
}

// pathlessErr strips the *os.PathError wrapper, whose text carries the file
// path, so a failed write or sync never puts a path in a log.
func pathlessErr(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// adoptSettledAfterScan gives the settled files of the scanned path a queue row
// (#1409). Best-effort and COUNTS-ONLY in its logs: a failure never aborts the
// scan. (The shared lyrics helpers it calls can log a path at debug level.)
func adoptSettledAfterScan(ctx context.Context, sqlDB *sql.DB, q *queue.DBQueue, lib models.Library, path, trigger string) {
	var dbFile string
	if err := sqlDB.QueryRowContext(ctx, `SELECT COALESCE(file, '') FROM pragma_database_list WHERE name = 'main'`).Scan(&dbFile); err != nil || dbFile == "" {
		slog.Warn("scan: settled-file adoption skipped: database file not resolvable", "library", lib.Name, "trigger", trigger)
		return
	}
	backup := &adoptBackup{f: lyrics.LazyBackupFile{Path: filepath.Join(filepath.Dir(dbFile), adoptSettledBackupName), What: "settled-file adoption backup"}}
	defer backup.f.Close()
	counts, err := (&scan.Adopter{Store: q, Backup: backup.append}).Run(ctx, lib.ID, path)
	if err != nil {
		slog.Warn("scan: settled-file adoption incomplete", "library", lib.Name, "trigger", trigger, "error", err)
	}
	if counts == (scan.AdoptCounts{}) {
		return
	}
	slog.Info("scan: adopted settled files into the queue",
		"library", lib.Name, "trigger", trigger,
		"inserted", counts.Inserted, "linked", counts.Linked, "not_settled", counts.NotSettled,
		"already_linked", counts.AlreadyLinked, "unexpected", counts.Unexpected,
		"no_sidecar", counts.NoSidecar, "unreadable", counts.Unreadable,
		"no_identity", counts.NoIdentity, "write_failed", counts.WriteFailed, "budget_hit", counts.BudgetHit)
}
