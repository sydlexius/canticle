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

// goneGrace is how long a row must have been gone inside a surviving directory,
// with nothing to relink it to, before the periodic sweep deletes it (#1262).
// Wall-clock, not a sweep count: serve restarts nightly and sweeps at startup,
// so a count would run out in days on one deployment and months on another. A
// week outlasts a slow re-rip and a weekend's outage. A constant on purpose:
// nothing has asked to tune it, and a shorter value only deletes sooner.
const goneGrace = 7 * 24 * time.Hour

// goneConfirmGap is how long after the sweep that first found a mark due (and
// recorded confirmed_at) a later sweep must run before it may delete: downtime
// is not absence, and a clock stepping a week makes every mark due at once.
const goneConfirmGap = time.Hour

// ageOutMaxPerSweep is the age-out's circuit breaker: more rows due in one
// sweep reads as a fault (a missing union-share branch), so none is deleted.
const ageOutMaxPerSweep = 50

// heldMark is the confirmed_at of a mark the tripped breaker holds: while over the cap, counted but not looked at.
const heldMark = -1

// goneMark is one prune_gone_since row, unix seconds; confirmed 0 is NULL.
type goneMark struct{ first, confirmed int64 }

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
// ASSUMPTION, verified on the Unraid user share (2026-10-03): the directory mtime seen through the library
// path moves on an in-folder change. A union/FUSE share that reports a
// directory's attributes from one branch only could hide a swap on another.
type dirMtimes struct {
	stat     func(string) (fs.FileInfo, error)
	now      time.Time
	scanMark int64    // MAX(scan_results.id) as this sweep began
	offline  []string // configured library roots that are unavailable
	stored   map[string]dirRecord
	seen     map[string]*dirObs
	// since is prune_gone_since (path -> when first seen gone and unrelinkable);
	// changed names the paths save must write back.
	since   map[string]goneMark
	changed map[string]bool
	heldN   int // rows whose mark the tripped breaker holds
	skipped int // those this sweep counted without examining
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
	d := &dirMtimes{stat: p.stat, now: p.now(), stored: map[string]dirRecord{}, seen: map[string]*dirObs{},
		since: map[string]goneMark{}, changed: map[string]bool{}}
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
		err = queryRows(ctx, p.db, `SELECT path, first_seen, COALESCE(confirmed_at, 0) FROM prune_gone_since`, nil, func(rows *sql.Rows) error {
			var path string
			var m goneMark
			if err := rows.Scan(&path, &m.first, &m.confirmed); err != nil {
				return err
			}
			// first_seen is never rewritten: one later than now is simply not due. A
			// confirmation later than now, or on a mark found not due, is dropped.
			if now := d.now.Unix(); m.confirmed != 0 && (m.confirmed > now || now-m.first < int64(goneGrace/time.Second)) {
				m.confirmed, d.changed[path] = 0, true
			}
			d.since[path] = m
			return nil
		})
	}
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
	// A row whose grace period has run out is stat'ed by itself: its directory
	// has not changed, so nothing else in it needs a look.
	if !o.examine && (!d.due(src) || d.holding(src)) {
		return false, false
	}
	_, err := d.stat(src)
	if errors.Is(err, fs.ErrNotExist) {
		o.gone++
	} else {
		d.forget(src) // seen present, or unreadable: the mark is dropped
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

// mark records when src was first seen gone with nothing to relink it to, if
// it has no mark yet. first_seen is never rewritten: an existing mark stands
// whatever the clock says now, and one later than now is simply not overdue.
// A mark is not STARTED while the directory's mtime is too recent to record
// (or later than now): under a clock that is behind, a just-removed file would
// get a first_seen years early and be overdue once the clock is corrected. The
// directory stays unrecorded, so the next sweep examines it and marks then.
func (d *dirMtimes) mark(src string) {
	if o := d.seen[filepath.Dir(src)]; o == nil || !o.record {
		return
	}
	if _, ok := d.since[src]; !ok {
		d.since[src], d.changed[src] = goneMark{first: d.now.Unix()}, true
	}
}

// due reports whether src has been gone for the whole grace period. A clock
// that stepped backward reads as not due.
func (d *dirMtimes) due(src string) bool {
	m, ok := d.since[src]
	return ok && d.now.Unix()-m.first >= int64(goneGrace/time.Second)
}

// confirm is the second phase for a due src still gone: the first such sweep
// records it and reports false; one goneConfirmGap or more later reports true.
func (d *dirMtimes) confirm(src string) bool {
	m := d.since[src]
	if m.confirmed == 0 {
		m.confirmed = d.now.Unix()
		d.since[src], d.changed[src] = m, true
		return false
	}
	return d.now.Unix()-m.confirmed >= int64(goneConfirmGap/time.Second)
}

// holding reports, and counts, a mark the tripped breaker is still holding.
func (d *dirMtimes) holding(src string) bool {
	if d.since[src].confirmed != heldMark || d.heldN <= ageOutMaxPerSweep {
		return false
	}
	d.skipped++
	return true
}

// confirmedDue reports whether src is due and was confirmed long enough ago, or held.
func (d *dirMtimes) confirmedDue(src string) bool {
	return d != nil && d.since[src].confirmed != 0 && d.now.Unix()-d.since[src].confirmed >= int64(goneConfirmGap/time.Second)
}

// aged starts src's grace period if it has none (mark, which may decline), and
// reports whether it is over.
func (d *dirMtimes) aged(src string) bool {
	d.mark(src)
	return d.due(src)
}

// forget drops src's mark: the file is back, the row was relinked or is held
// for a present sibling or same-identity file, or the delete was refused. A
// refused or held row is not marked again until its directory is examined
// again (its mtime moves, or a scan inserts). A nil d (Exact) is a no-op.
func (d *dirMtimes) forget(src string) {
	if d == nil {
		return
	}
	if _, ok := d.since[src]; ok {
		delete(d.since, src)
		d.changed[src] = true
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
	for src := range d.changed {
		if m, ok := d.since[src]; ok {
			_, err = tx.ExecContext(ctx, `INSERT INTO prune_gone_since (path, first_seen, confirmed_at) VALUES (?, ?, NULLIF(?, 0))
                ON CONFLICT(path) DO UPDATE SET first_seen = excluded.first_seen, confirmed_at = excluded.confirmed_at`, src, m.first, m.confirmed)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM prune_gone_since WHERE path = ?`, src)
		}
		if err != nil {
			return err
		}
	}
	// A path no row names any more (deleted, or relinked by another pass) keeps
	// no grace period for a later row at the same path to inherit.
	if _, err := tx.ExecContext(ctx, `DELETE FROM prune_gone_since
        WHERE NOT EXISTS (SELECT 1 FROM scan_results WHERE file_path = prune_gone_since.path)
          AND NOT EXISTS (SELECT 1 FROM work_queue WHERE source_path = prune_gone_since.path)`); err != nil {
		return err
	}
	return tx.Commit()
}
