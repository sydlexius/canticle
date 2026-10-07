// Package instrumentalmark marks a track instrumental by hand: it backs up the
// lyric files beside the audio, settles the work_queue row as a protected
// manual instrumental, and writes the manual marker in their place.
package instrumentalmark

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	dbpkg "github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/sidecar"
)

var (
	// ErrNoLibrary is returned when none of the row's output directories sits
	// inside a library root linked to the row. Nothing is touched.
	ErrNoLibrary = errors.New("instrumentalmark: output is not inside a library root linked to the row")
	// ErrSymlinkedSidecar is returned when a lyric file is a symlink: it can
	// be neither followed nor backed up, so the mark refuses before changing
	// anything.
	ErrSymlinkedSidecar = errors.New("instrumentalmark: a lyric file is a symlink")
	// ErrNoBackupSink is returned when a non-dry-run mark has a lyric file to
	// replace but Options.Report is nil: the replacement would leave no record.
	ErrNoBackupSink = errors.New("instrumentalmark: lyric files to replace but no Report to back them up")
	// ErrMarkWithdrawn is returned when the row's mark was cleared (an unmark)
	// after this call settled it; nothing further is written.
	ErrMarkWithdrawn = errors.New("instrumentalmark: the mark was withdrawn while marking; no further files were written")
)

// Outcome is the result class of a Mark call.
type Outcome string

// Outcomes: marked (files replaced, row settled), already_marked (row and
// marker both present, nothing written), dry_run (nothing written), unmarked
// (an Unmark removed the mark and re-queued the row) and not_marked (an Unmark
// found no mark on the row and changed nothing).
const (
	OutcomeMarked        Outcome = "marked"
	OutcomeAlreadyMarked Outcome = "already_marked"
	OutcomeDryRun        Outcome = "dry_run"
	OutcomeUnmarked      Outcome = "unmarked"
	OutcomeNotMarked     Outcome = "not_marked"
)

// Options configures one Mark call.
type Options struct {
	// DryRun computes the result and writes nothing, Report is not called.
	DryRun bool
	// Report receives one Record per file about to be replaced, BEFORE
	// anything changes. It must make the record durable (see AppendRecord);
	// an error aborts the mark with nothing changed. Required (else
	// ErrNoBackupSink) for a non-dry-run mark that has a file to replace.
	Report func(Record) error
}

// Result describes a Mark or Unmark call. FilesBackedUp counts the files that
// were (or in a dry run would be) backed up: for Mark the lyric files it
// replaces, for Unmark the manual marker files it removes.
type Result struct {
	Outcome       Outcome
	FilesBackedUp int
}

// Marker marks tracks instrumental. The writer's roots confine its writes and
// it should carry the serve process's selfwrite registry, which records every
// path the writer writes or removes.
type Marker struct {
	db    *sql.DB
	w     *lyrics.LRCWriter
	locks rowLocks
}

// New returns a Marker over db and writer w. The caller wires the writer's
// selfwrite registry; a separate CLI process cannot share the serve process's
// registry, so the watcher there rescans the settled row, which is harmless.
func New(db *sql.DB, w *lyrics.LRCWriter) *Marker { return &Marker{db: db, w: w} }

type target struct {
	artist, title, album string
	status               string
	marked               bool
	outputs              []models.OutputPath
	keys                 [][2]string
}

const busyAttempts = 5

// Mark marks work_queue row id instrumental by hand. Order, so that no
// failure leaves a claimed success: (1) refuse an in-flight or unplaceable
// row; (2) back up every lyric file the marker will replace via opts.Report;
// (3) one transaction settles the row and invalidates the track's cache entry
// (a surviving entry would let the next scan resurrect the old lyrics); (4)
// write the marker. A step-4 failure before ANY marker was written unmarks a
// row this call marked, which re-queues it so a scan rewrites the files
// (originals remain in the backup record). Once any marker is written, or the
// post-settle re-inventory fails, the row stays marked and the error says so:
// calling Mark again on a marked row without every marker repairs it.
func (m *Marker) Mark(ctx context.Context, id int64, opts Options) (Result, error) {
	defer m.locks.lock(id)() // serialized with Unmark on this row, see Unmark
	t, err := m.load(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if t.status == queue.StatusProcessing {
		return Result{}, queue.ErrManualInstrumentalInFlight
	}
	dirs, err := m.resolveDirs(ctx, id, &t)
	if err != nil {
		return Result{}, err
	}
	files, markerPresent, err := inventory(t, dirs)
	if err != nil {
		return Result{}, err
	}
	// A no-op needs nothing left to back up or replace: a lyric file beside the
	// marker (a .lrc, or a case variant on a case-sensitive filesystem) is work.
	if t.marked && markerPresent && len(files) == 0 {
		return Result{Outcome: OutcomeAlreadyMarked}, nil
	}
	if opts.DryRun {
		return Result{Outcome: OutcomeDryRun, FilesBackedUp: len(files)}, nil
	}
	if len(files) > 0 && opts.Report == nil {
		return Result{}, ErrNoBackupSink
	}
	reported := make(map[string]bool)
	for _, p := range files {
		if rerr := backup(OpMark, id, p, opts.Report, reported); rerr != nil {
			return Result{}, fmt.Errorf("instrumentalmark: backup of work item %d failed, nothing changed: %w", id, rerr)
		}
	}
	changed, err := m.settle(ctx, id, t)
	if err != nil {
		return Result{}, err
	}
	// Second inventory: a file that appeared after the first (a hand-written
	// .txt, a .lrc a worker finished) is backed up too. A failure here leaves
	// the row marked with no marker written; a retry of Mark repairs it. A file
	// created between this inventory and the writer's rename can still be lost:
	// there is no per-stem lock.
	//
	// The mark can also be withdrawn (an unmark) between settle and the writes.
	// It is re-read before the late backups and before each marker write, which
	// narrows the window to the gap between that read and the write; it does not
	// close it.
	late, _, err := inventory(t, dirs)
	if err != nil {
		return Result{}, fmt.Errorf("instrumentalmark: work item %d is marked but no marker was written; retry the mark to repair: %w", id, err)
	}
	if len(late) > 0 {
		if err := m.stillMarked(ctx, id); err != nil {
			return Result{}, err
		}
	}
	for _, p := range late {
		if rerr := backup(OpMark, id, p, opts.Report, reported); rerr != nil {
			return Result{}, fmt.Errorf("instrumentalmark: work item %d is marked but a late file could not be backed up and no marker was written; retry the mark to repair: %w", id, rerr)
		}
	}
	written := 0
	song := models.Song{
		Track:       models.Track{ArtistName: t.artist, TrackName: t.title, AlbumName: t.album, Instrumental: 1},
		WinningLane: lyrics.ManualLaneName,
	}
	for i, o := range t.outputs {
		if err := m.stillMarked(ctx, id); err != nil {
			return Result{}, err
		}
		if werr := m.w.WriteManualMarker(song, o.Filename, dirs[i]); werr != nil {
			werr = fmt.Errorf("instrumentalmark: write marker for work item %d: %w", id, stripPath(werr))
			if written > 0 {
				return Result{}, fmt.Errorf("%w (%d of %d markers written; the row stays marked, retry the mark to repair)", werr, written, len(t.outputs))
			}
			if changed {
				if _, uerr := m.unmark(ctx, id); uerr != nil {
					slog.Warn("instrumentalmark: could not unmark after a failed marker write; calling Mark again repairs it", "id", id, "error", uerr)
					werr = errors.Join(werr, uerr)
				}
			}
			return Result{}, werr
		}
		written++
	}
	// The writer removes the opposite-extension variants but not a same-extension
	// one (a Song.TXT beside song.txt on a case-sensitive filesystem). Remove what
	// was backed up and is still there, after the backup and the settle.
	for p := range reported {
		if sidecar.KindOf(p) != sidecar.KindUnsynced {
			continue
		}
		if rerr := m.w.RemoveReplacedSidecar(p); rerr != nil {
			return Result{}, fmt.Errorf("instrumentalmark: work item %d is marked but a backed-up lyric file could not be removed; retry the mark to repair: %w", id, stripPath(rerr))
		}
	}
	return Result{Outcome: OutcomeMarked, FilesBackedUp: len(reported)}, nil
}

// stillMarked returns ErrMarkWithdrawn when the row no longer carries a mark.
func (m *Marker) stillMarked(ctx context.Context, id int64) error {
	var at sql.NullString
	if err := m.db.QueryRowContext(ctx, `SELECT manual_instrumental_at FROM work_queue WHERE id = ?`, id).Scan(&at); err != nil {
		return fmt.Errorf("instrumentalmark: re-read mark for work item %d: %w", id, err)
	}
	if !at.Valid {
		return ErrMarkWithdrawn
	}
	return nil
}

func (m *Marker) load(ctx context.Context, id int64) (target, error) {
	var t target
	var outdir, filename string
	var outputPaths sql.NullString
	var markedAt sql.NullString
	err := m.db.QueryRowContext(ctx,
		`SELECT artist, title, album, status, outdir, filename, output_paths, manual_instrumental_at
           FROM work_queue WHERE id = ?`, id).
		Scan(&t.artist, &t.title, &t.album, &t.status, &outdir, &filename, &outputPaths, &markedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return target{}, queue.ErrManualInstrumentalNotFound
	}
	if err != nil {
		return target{}, fmt.Errorf("instrumentalmark: load work item %d: %w", id, err)
	}
	t.marked = markedAt.Valid
	if outputPaths.Valid && outputPaths.String != "" {
		if err := json.Unmarshal([]byte(outputPaths.String), &t.outputs); err != nil {
			return target{}, fmt.Errorf("instrumentalmark: work item %d output_paths: %w", id, err)
		}
	}
	if len(t.outputs) == 0 {
		t.outputs = []models.OutputPath{{Outdir: outdir, Filename: filename}}
	}
	return t, nil
}

// resolveDirs returns each output's symlink-resolved directory, confined to a
// library root linked to the row, and fills t.keys with the (artist, title)
// cache keys of the scan_results rows linked to it.
func (m *Marker) resolveDirs(ctx context.Context, id int64, t *target) ([]string, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT l.path, sr.artist, sr.title FROM libraries l
           JOIN scan_results sr ON sr.library_id = l.id
           JOIN work_queue_scan_results j ON j.scan_result_id = sr.id
          WHERE j.work_queue_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("instrumentalmark: links for work item %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var roots []string
	t.keys = append(t.keys, [2]string{t.artist, t.title})
	for rows.Next() {
		var r, a, ti string
		if err := rows.Scan(&r, &a, &ti); err != nil {
			return nil, fmt.Errorf("instrumentalmark: scan link: %w", err)
		}
		roots = append(roots, r)
		t.keys = append(t.keys, [2]string{a, ti})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("instrumentalmark: links rows: %w", err)
	}
	dirs := make([]string, len(t.outputs))
	for i, o := range t.outputs {
		for _, r := range roots {
			if d, ok := pathutil.ResolveWithinRoot(r, o.Outdir); ok {
				dirs[i] = d
				break
			}
		}
		if dirs[i] == "" {
			return nil, ErrNoLibrary
		}
	}
	return dirs, nil
}

// inventory lists the existing lyric files the marker will replace (every
// case variant of the .lrc and .txt, plus an owned .elrc) and reports whether
// every output already carries a manual marker. A manual marker itself is not
// a file to back up.
func inventory(t target, dirs []string) (files []string, allMarked bool, err error) {
	seen := make(map[string]bool)
	allMarked = true
	for i, o := range t.outputs {
		l := sidecar.List(dirs[i])
		var txtFp, lrcFp string
		for _, synced := range []bool{true, false} {
			name, nerr := lyrics.SidecarName(t.artist, t.title, o.Filename, synced)
			if nerr != nil {
				return nil, false, fmt.Errorf("instrumentalmark: sidecar name: %w", nerr)
			}
			fp := filepath.Join(dirs[i], name)
			if synced {
				lrcFp = fp
			} else {
				txtFp = fp
			}
			if len(l.SymlinkVariants(fp)) > 0 {
				return nil, false, ErrSymlinkedSidecar
			}
			for _, v := range l.Variants(fp) {
				fi, serr := os.Lstat(v)
				if serr != nil {
					return nil, false, fmt.Errorf("instrumentalmark: stat lyric file: %w", stripPath(serr))
				}
				if fi.Mode()&os.ModeSymlink != 0 {
					return nil, false, ErrSymlinkedSidecar
				}
				if !synced && lyrics.ManualMarkerOnDisk(v) {
					continue
				}
				if !seen[v] {
					seen[v] = true
					files = append(files, v)
				}
			}
		}
		// Every owned .elrc variant the writer will remove, with or without a
		// .lrc beside it; the writer's own predicate, not a copy of its rule.
		for _, c := range lyrics.OwnedCompanions(lrcFp, l) {
			if !seen[c] {
				seen[c] = true
				files = append(files, c)
			}
		}
		if !lyrics.ManualMarkerOnDisk(txtFp) {
			allMarked = false
		}
	}
	return files, allMarked, nil
}

// backup reports path once; reported dedupes across the two inventories.
func backup(op string, id int64, path string, report func(Record) error, reported map[string]bool) error {
	if reported[path] {
		return nil
	}
	if report == nil {
		return ErrNoBackupSink
	}
	rec, err := readRecord(op, id, path)
	if err == nil {
		err = report(rec)
	}
	if err == nil {
		reported[path] = true
	}
	return err
}

// readRecord captures path's bytes for the backup through one no-follow
// handle, so a file swapped for a symlink after the inventory is refused.
// Errors never carry the path (a sidecar path is private library metadata).
func readRecord(op string, id int64, path string) (Record, error) {
	b, err := lyrics.ReadRegularNoFollow(path, MaxBackupBytes)
	if err != nil {
		return Record{}, fmt.Errorf("read lyric file (not regular, over the %d-byte backup limit, or unreadable): %w", MaxBackupBytes, stripPath(err))
	}
	return Record{Op: op, WorkItemID: id, Path: path, Content: b}, nil
}

// stripPath drops the path an os error carries, keeping only its cause.
func stripPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

// settle marks the row and invalidates every cache key a re-scan could look up
// for it, in one transaction.
func (m *Marker) settle(ctx context.Context, id int64, t target) (changed bool, err error) {
	err = dbpkg.RetryOnBusy(ctx, busyAttempts, func() error {
		tx, berr := m.db.BeginTx(ctx, nil)
		if berr != nil {
			return fmt.Errorf("instrumentalmark: begin: %w", berr)
		}
		defer func() { _ = tx.Rollback() }()
		c, merr := queue.MarkManualInstrumentalTx(ctx, tx, id, time.Now())
		if merr != nil {
			return merr
		}
		for _, k := range t.keys {
			if _, ierr := cache.Invalidate(ctx, tx, k[0], k[1]); ierr != nil {
				return ierr
			}
		}
		if cerr := tx.Commit(); cerr != nil {
			return fmt.Errorf("instrumentalmark: commit: %w", cerr)
		}
		changed = c
		return nil
	})
	return changed, err
}

func (m *Marker) unmark(ctx context.Context, id int64) (changed bool, err error) {
	err = dbpkg.RetryOnBusy(ctx, busyAttempts, func() error {
		tx, berr := m.db.BeginTx(ctx, nil)
		if berr != nil {
			return fmt.Errorf("instrumentalmark: begin unmark: %w", berr)
		}
		defer func() { _ = tx.Rollback() }()
		c, uerr := queue.UnmarkManualInstrumentalTx(ctx, tx, id, time.Now())
		if uerr != nil {
			return uerr
		}
		if cerr := tx.Commit(); cerr != nil {
			return fmt.Errorf("instrumentalmark: commit unmark: %w", cerr)
		}
		changed = c
		return nil
	})
	return changed, err
}
