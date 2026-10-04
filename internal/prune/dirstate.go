package prune

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	dbpkg "github.com/sydlexius/canticle/internal/db"
)

// dirSettleMargin is how old a directory's mtime must be before it is recorded
// as examined. A younger (or future, under clock skew) mtime is never recorded,
// so the directory is examined again next sweep: a change landing in the same
// timestamp granule as the examination, on a filesystem with coarse mtimes,
// cannot hide behind an mtime that was already stored.
const dirSettleMargin = time.Minute

// dirMtimes is the Directory-granularity sweep's change detector (#1262). A
// recorded directory costs one stat; its row files are stat'ed exactly only
// when it is unrecorded, its mtime differs from the stored one (an in-folder
// swap is a delete plus a create, which moves it), or it was recorded with a
// row still gone and a scan has indexed something since (scan_results ids only
// grow, so MAX(id) is the mark): that is how a replacement that was already on
// disk becomes relinkable without the mtime moving. A directory with a gone
// row is therefore recorded too and converges; one whose mtime is under a
// minute old or in the future, whose row was unreadable or in flight, or whose
// relink a worker raced is left unrecorded and examined again next sweep. A
// relink declined because another queue row owns the sibling (or its identity
// differs) is recorded like any other: it is retried only on a directory
// change, a scan insert, or `scan reconcile-paths`, not when the owner goes.
//
// ASSUMPTION, not testable here: the directory mtime seen through the library
// path moves on an in-folder change. A union/FUSE share that reports a
// directory's attributes from one branch only could hide a swap on another.
type dirMtimes struct {
	stat     func(string) (fs.FileInfo, error)
	now      time.Time
	scanMark int64    // MAX(scan_results.id) as this sweep began
	offline  []string // configured library roots that are unavailable
	stored   map[string]dirRecord
	seen     map[string]*dirObs
}

// dirRecord is one stored row; goneMark is NULL when every row was present.
type dirRecord struct {
	mtime    int64
	goneMark sql.NullInt64
}

type dirObs struct {
	exists  bool  // false only on a definitive not-exist
	examine bool  // stat this directory's row files
	record  bool  // the mtime is old enough to store
	gone    int   // row files definitively gone and not relinked since
	retry   bool  // a row was unreadable or in flight: never recorded
	mtime   int64 // UnixNano
}

func (p *Pruner) loadDirState(ctx context.Context) (*dirMtimes, error) {
	d := &dirMtimes{stat: p.stat, now: p.now(), stored: map[string]dirRecord{}, seen: map[string]*dirObs{}}
	err := queryRows(ctx, p.db, `SELECT dir, mtime_ns, gone_scan_id FROM prune_dir_state`, nil, func(rows *sql.Rows) error {
		var dir string
		var rec dirRecord
		if err := rows.Scan(&dir, &rec.mtime, &rec.goneMark); err != nil {
			return err
		}
		d.stored[dir] = rec
		return nil
	})
	if err == nil {
		err = p.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM scan_results`).Scan(&d.scanMark)
	}
	if err != nil {
		return nil, fmt.Errorf("prune: load directory state: %w", err)
	}
	return d, nil
}

func (d *dirMtimes) observe(dir string) *dirObs {
	if o, ok := d.seen[dir]; ok {
		return o
	}
	o := &dirObs{exists: true}
	d.seen[dir] = o
	fi, err := d.stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		o.exists = false
	case err != nil:
		// Unreadable is not gone and not changed: touch nothing, record nothing.
	default:
		o.mtime = fi.ModTime().UnixNano()
		rec, ok := d.stored[dir]
		o.examine = !ok || rec.mtime != o.mtime || (rec.goneMark.Valid && d.scanMark > rec.goneMark.Int64)
		o.record = !fi.ModTime().After(d.now.Add(-dirSettleMargin))
	}
	return o
}

// gone reports whether src is gone, and whether it is gone INSIDE a surviving
// directory (inFolder) rather than along with it. Only a definitive not-exist
// counts; any other stat error keeps the row and its directory unrecorded.
func (d *dirMtimes) gone(src string) (isGone, inFolder bool) {
	o := d.observe(filepath.Dir(src))
	if !o.exists {
		return true, false
	}
	if !o.examine {
		return false, false
	}
	_, err := d.stat(src)
	if errors.Is(err, fs.ErrNotExist) {
		o.gone++
	}
	o.retry = o.retry || (err != nil && !errors.Is(err, fs.ErrNotExist))
	return errors.Is(err, fs.ErrNotExist), true
}

// relinked takes src back out of its directory's gone count: a row the sweep
// relinked is no longer gone, so a directory whose last gone row moved is
// stored without the scan mark and is not re-examined on the next scan insert.
// A nil d (Exact granularity) is a no-op.
func (d *dirMtimes) relinked(src string) {
	if d != nil {
		d.seen[filepath.Dir(src)].gone--
	}
}

// retry keeps src's directory unrecorded, so it is examined again next sweep.
// A nil d (Exact granularity) is a no-op.
func (d *dirMtimes) retry(src string) {
	if d != nil {
		d.seen[filepath.Dir(src)].retry = true
	}
}

// save stores every examined directory whose mtime has settled, with the scan
// mark when a row in it is still gone, and drops the state of a directory that
// vanished, must be retried, or no longer has any row (a removed library's
// included; only state under a configured root that is unavailable is kept).
// Retried whole on SQLITE_BUSY.
func (d *dirMtimes) save(ctx context.Context, db *sql.DB) error {
	return dbpkg.RetryBatchTx(ctx, "prune directory state", func() error {
		if err := d.saveOnce(ctx, db); err != nil {
			return fmt.Errorf("prune: save directory state: %w", err)
		}
		return nil
	})
}

func (d *dirMtimes) saveOnce(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for dir, o := range d.seen {
		switch {
		case o.exists && !o.examine:
			continue // unchanged, or its stat failed: the stored row stands
		case o.exists && o.record && !o.retry:
			var mark any
			if o.gone > 0 {
				mark = d.scanMark
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO prune_dir_state (dir, mtime_ns, gone_scan_id) VALUES (?, ?, ?)
                ON CONFLICT(dir) DO UPDATE SET mtime_ns = excluded.mtime_ns, gone_scan_id = excluded.gone_scan_id`, dir, o.mtime, mark)
		default:
			_, err = tx.ExecContext(ctx, `DELETE FROM prune_dir_state WHERE dir = ?`, dir)
		}
		if err != nil {
			return err
		}
	}
	for dir := range d.stored {
		if _, ok := d.seen[dir]; ok || underAvailableRoot(dir, d.offline) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM prune_dir_state WHERE dir = ?`, dir); err != nil {
			return err
		}
	}
	return tx.Commit()
}
