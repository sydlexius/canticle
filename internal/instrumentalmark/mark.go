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
)

// Outcome is the result class of a Mark call.
type Outcome string

// Outcomes: marked (files replaced, row settled), already_marked (row and
// marker both present, nothing written) and dry_run (nothing written).
const (
	OutcomeMarked        Outcome = "marked"
	OutcomeAlreadyMarked Outcome = "already_marked"
	OutcomeDryRun        Outcome = "dry_run"
)

// Options configures one Mark call.
type Options struct {
	// DryRun computes the result and writes nothing, Report is not called.
	DryRun bool
	// Report receives one Record per file about to be replaced, BEFORE
	// anything changes. It must make the record durable (see AppendRecord);
	// an error aborts the mark with nothing changed.
	Report func(Record) error
}

// Result describes a Mark call. FilesBackedUp counts the files that were (or
// in a dry run would be) replaced.
type Result struct {
	Outcome       Outcome
	FilesBackedUp int
}

// Marker marks tracks instrumental. The writer's roots confine its writes and
// it should carry the serve process's selfwrite registry, which records every
// path the writer writes or removes.
type Marker struct {
	db *sql.DB
	w  *lyrics.LRCWriter
}

// New returns a Marker over db and writer w.
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
// write the marker. A step-4 failure unmarks a row this call marked, which
// re-queues it so a scan rewrites the files (originals remain in the backup
// record); calling Mark again on a marked row with no marker repairs it.
func (m *Marker) Mark(ctx context.Context, id int64, opts Options) (Result, error) {
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
	if t.marked && markerPresent {
		return Result{Outcome: OutcomeAlreadyMarked}, nil
	}
	if opts.DryRun {
		return Result{Outcome: OutcomeDryRun, FilesBackedUp: len(files)}, nil
	}
	for _, p := range files {
		rec, rerr := readRecord(id, p)
		if rerr == nil && opts.Report != nil {
			rerr = opts.Report(rec)
		}
		if rerr != nil {
			return Result{}, fmt.Errorf("instrumentalmark: backup of work item %d failed, nothing changed: %w", id, rerr)
		}
	}
	changed, err := m.settle(ctx, id, t)
	if err != nil {
		return Result{}, err
	}
	song := models.Song{
		Track:       models.Track{ArtistName: t.artist, TrackName: t.title, AlbumName: t.album, Instrumental: 1},
		WinningLane: lyrics.ManualLaneName,
	}
	for i, o := range t.outputs {
		if werr := m.w.WriteManualMarker(song, o.Filename, dirs[i]); werr != nil {
			werr = fmt.Errorf("instrumentalmark: write marker for work item %d: %w", id, werr)
			if changed {
				if uerr := m.unmark(ctx, id); uerr != nil {
					slog.Warn("instrumentalmark: could not unmark after a failed marker write; calling Mark again repairs it", "id", id, "error", uerr)
					werr = errors.Join(werr, uerr)
				}
			}
			return Result{}, werr
		}
	}
	return Result{Outcome: OutcomeMarked, FilesBackedUp: len(files)}, nil
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
		var txtFp string
		for _, synced := range []bool{true, false} {
			name, nerr := lyrics.SidecarName(t.artist, t.title, o.Filename, synced)
			if nerr != nil {
				return nil, false, fmt.Errorf("instrumentalmark: sidecar name: %w", nerr)
			}
			fp := filepath.Join(dirs[i], name)
			if !synced {
				txtFp = fp
			}
			for _, v := range l.Variants(fp) {
				fi, serr := os.Lstat(v)
				if serr != nil {
					return nil, false, fmt.Errorf("instrumentalmark: stat lyric file: %w", serr)
				}
				if fi.Mode()&os.ModeSymlink != 0 {
					return nil, false, ErrSymlinkedSidecar
				}
				cands := []string{v}
				if synced {
					if c := lyrics.OwnedCompanionOf(v); c != "" {
						cands = append(cands, c)
					}
				} else if lyrics.ManualMarkerOnDisk(v) {
					continue
				}
				for _, c := range cands {
					if !seen[c] {
						seen[c] = true
						files = append(files, c)
					}
				}
			}
		}
		if !lyrics.ManualMarkerOnDisk(txtFp) {
			allMarked = false
		}
	}
	return files, allMarked, nil
}

// readRecord captures path's bytes for the backup, never following a symlink.
func readRecord(id int64, path string) (Record, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Record{}, fmt.Errorf("stat %q: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return Record{}, fmt.Errorf("%q is not a regular file", path)
	}
	if fi.Size() > MaxBackupBytes {
		return Record{}, fmt.Errorf("%q is over the %d-byte backup limit", path, MaxBackupBytes)
	}
	b, err := os.ReadFile(path) //nolint:gosec // reason: G304: path is a lyric sidecar confined to a library root and Lstat'ed regular above
	if err != nil {
		return Record{}, fmt.Errorf("read %q: %w", path, err)
	}
	return Record{Op: OpMark, WorkItemID: id, Path: path, Content: b}, nil
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

func (m *Marker) unmark(ctx context.Context, id int64) error {
	return dbpkg.RetryOnBusy(ctx, busyAttempts, func() error {
		tx, err := m.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("instrumentalmark: begin unmark: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := queue.UnmarkManualInstrumentalTx(ctx, tx, id, time.Now()); err != nil {
			return err
		}
		return tx.Commit()
	})
}
