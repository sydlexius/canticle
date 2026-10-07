package lyricblock

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	dbpkg "github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/selfwrite"
)

var (
	// ErrBusy is returned when the work item is 'processing': a worker holds it.
	ErrBusy = errors.New("lyricblock: work item is in flight")
	// ErrNoSidecar is returned when the work item has no lyric file on disk.
	ErrNoSidecar = errors.New("lyricblock: no lyric file on disk for the work item")
	// ErrNotFound is returned when no work item or block has the given id.
	ErrNotFound = errors.New("lyricblock: not found")
	// ErrManualInstrumental is returned for a row an operator marked instrumental
	// by hand: its marker is not a lyric, and unmarking is that feature's action.
	ErrManualInstrumental = errors.New("lyricblock: work item is marked instrumental by hand")
	// ErrNotMarkable is returned for a 'failed' or 'unavailable' row. The queue
	// revives those only through their own paths (Retry, RecheckRetired); a mark
	// would delete the files and strand the row. Nothing is touched.
	ErrNotMarkable = errors.New("lyricblock: work item is failed or unavailable; revive it first")
)

const (
	opMark       = "mark-wrong"
	busyAttempts = 5
)

// Service is the operator action layer over the block Store: mark a track's
// lyrics wrong, unblock. It owns no CLI types.
type Service struct {
	db    *sql.DB
	store *Store
	sw    *selfwrite.Registry
	log   *slog.Logger
}

// New returns a Service over db. sw records the paths the service unlinks so
// the watcher ignores them (nil is a no-op: a CLI process cannot share the serve
// process's registry, and the watcher then rescans the settled row, harmlessly).
// A nil logger uses slog.Default(). It logs ids and counts only, never a name,
// path or lyric text above Debug.
func New(db *sql.DB, log *slog.Logger, sw *selfwrite.Registry) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, store: NewStore(db, log), sw: sw, log: log}
}

// MarkRequest asks to mark one work item's on-disk lyrics wrong.
type MarkRequest struct {
	WorkItemID int64
	// Roots are the library roots every output directory must sit inside.
	Roots []string
	// DryRun computes the result and writes nothing; Report is not called.
	DryRun bool
	// Report receives one Backup per file about to be removed (again for a file
	// that appeared or changed since), BEFORE it is removed. It must make the
	// record durable (see AppendBackup); an error aborts the mark.
	Report func(Backup) error
}

// MarkResult holds counts only. Files is the number of lyric files found (and,
// unless DryRun, backed up); Removed how many were unlinked; NewBlocks the
// fingerprints newly blocked; Reopened whether a settled row went back to
// pending; CacheInvalidated the lyrics_cache rows deleted.
type MarkResult struct {
	DryRun           bool
	Files            int
	Removed          int
	NewBlocks        int
	Reopened         bool
	CacheInvalidated int
}

type target struct {
	artist, title, status, lane, upstream string
	manual                                bool
	outputs                               []models.OutputPath
	// key is the row's own work_queue.artist_key/title_key, the identity blocks
	// are stored under. cacheKeys are the identities to un-cache: the row's and
	// each linked scan result's (the cache is keyed by those, not by key).
	key       [2]string
	cacheKeys [][2]string
	// meta is the row state Mark clears, carried on every Backup.
	meta map[string]string
}

// Mark blocks the on-disk lyrics of a work item, removes them and reopens the
// row. Order: refuse an in-flight, hand-marked, failed/unavailable or unplaceable
// row; Report every file; ONE transaction inserts the blocks, invalidates the
// cache and clears the row's edit marks (the row stays settled, so no worker can
// claim it); re-read EVERY file and Report and block any that is new or changed;
// unlink; a second transaction reopens the row and clears its word/upgrade state.
//
// A failure before the first transaction changes nothing; after it, the blocks
// stand and the row is settled with whatever files remain. Calling Mark again
// completes: inserts are idempotent, and with no file left it just reopens a
// settled row whose identity is already blocked. A pending or deferred row can
// be claimed mid-mark; the re-read narrows that to the read-to-unlink gap.
//
// The cleared edit state rides on each Backup (Meta). A hand-placed instrumental
// marker is left alone. A track marked from a bilingual .lrc blocks the
// bilingual form only; the same words fetched without a translation are not
// blocked.
func (s *Service) Mark(ctx context.Context, req MarkRequest) (MarkResult, error) {
	id := req.WorkItemID
	t, err := s.load(ctx, id)
	if err != nil {
		return MarkResult{}, err
	}
	switch {
	case t.manual:
		return MarkResult{}, ErrManualInstrumental
	case t.status == queue.StatusProcessing:
		return MarkResult{}, ErrBusy
	case t.status == queue.StatusFailed || t.status == queue.StatusUnavailable:
		return MarkResult{}, ErrNotMarkable
	}
	outs, err := resolveOutputs(req.Roots, t.outputs)
	if err != nil {
		return MarkResult{}, err
	}
	files, err := Inventory(t.artist, t.title, outs)
	if err != nil {
		return MarkResult{}, err
	}
	if len(files) == 0 {
		// A mark that died between the unlink and the reopen: finish it.
		blocked, berr := s.anyBlocked(ctx, t)
		if berr != nil {
			return MarkResult{}, berr
		}
		if !blocked || t.status != queue.StatusDone {
			return MarkResult{}, ErrNoSidecar
		}
		res := MarkResult{DryRun: req.DryRun}
		if !req.DryRun {
			err = s.reopen(ctx, id, &res)
		}
		return res, err
	}
	recs, err := ReadAll(opMark, id, t.meta, files)
	if err != nil {
		return MarkResult{}, fmt.Errorf("lyricblock: work item %d: %w", id, err)
	}
	fps := newFingerprints(recs, nil)
	if len(fps) == 0 {
		return MarkResult{}, fmt.Errorf("lyricblock: work item %d: %w", id, ErrNoFingerprint)
	}
	if req.DryRun {
		return MarkResult{DryRun: true, Files: len(files)}, nil
	}
	reported := reportedSet{}
	if rerr := ReportAll(req.Report, recs, reported); rerr != nil {
		return MarkResult{}, fmt.Errorf("lyricblock: backup for work item %d failed, nothing changed: %w", id, rerr)
	}
	var res MarkResult
	err = dbpkg.RetryOnBusy(ctx, busyAttempts, func() error {
		var terr error
		res, terr = s.block(ctx, id, t, fps, true)
		return terr
	})
	if err != nil {
		return MarkResult{}, err
	}
	res.Files = len(files)
	late, err := Inventory(t.artist, t.title, outs)
	if err != nil {
		return res, fmt.Errorf("lyricblock: work item %d is blocked but its files were not removed; retry the mark: %w", id, err)
	}
	lateRecs, rerr := ReadAll(opMark, id, t.meta, late)
	if rerr == nil {
		rerr = ReportAll(req.Report, lateRecs, reported)
	}
	if rerr != nil {
		return res, fmt.Errorf("lyricblock: work item %d is blocked but a file could not be backed up and nothing was removed; retry the mark: %w", id, rerr)
	}
	if extra := newFingerprints(lateRecs, fps); len(extra) > 0 {
		var more MarkResult
		err = dbpkg.RetryOnBusy(ctx, busyAttempts, func() error {
			var terr error
			more, terr = s.block(ctx, id, t, extra, false)
			return terr
		})
		if err != nil {
			return res, fmt.Errorf("lyricblock: work item %d is blocked but a new file could not be blocked and nothing was removed; retry the mark: %w", id, err)
		}
		res.NewBlocks += more.NewBlocks
	}
	n, err := RemoveFiles(late, s.sw)
	res.Removed = n
	if err != nil {
		return res, fmt.Errorf("lyricblock: work item %d is blocked but only %d of %d lyric files were removed; retry the mark to finish: %w", id, n, len(late), err)
	}
	if err := s.reopen(ctx, id, &res); err != nil {
		return res, fmt.Errorf("lyricblock: work item %d: files removed and blocked but the row was not reopened; retry the mark: %w", id, err)
	}
	s.log.Info("lyricblock: marked wrong", "work_item_id", id, "files", res.Removed, "new_blocks", res.NewBlocks, "reopened", res.Reopened)
	return res, nil
}

// block is Mark's first transaction: the blocks and the cache invalidation
// commit together. With clear it first clears the row's edit marks, guarded so
// the row it changes is the row it judged (not in flight, not hand-marked).
func (s *Service) block(ctx context.Context, id int64, t target, fps []string, clear bool) (MarkResult, error) {
	var res MarkResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("lyricblock: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if clear {
		if err := guardedUpdate(ctx, tx, id, clearEditMarks); err != nil {
			return res, err
		}
	}
	for _, fp := range fps {
		added, aerr := s.store.Add(ctx, tx, Block{ArtistKey: t.key[0], TitleKey: t.key[1], Fingerprint: fp, WorkQueueID: id, Lane: t.lane, Upstream: t.upstream})
		if aerr != nil {
			return res, aerr
		}
		if added {
			res.NewBlocks++
		}
	}
	for _, k := range t.cacheKeys {
		if !clear {
			break
		}
		n, ierr := cache.Invalidate(ctx, tx, k[0], k[1])
		if ierr != nil {
			return res, fmt.Errorf("lyricblock: invalidate cache for work item %d: %w", id, ierr)
		}
		res.CacheInvalidated += n
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("lyricblock: commit: %w", err)
	}
	return res, nil
}

// reopen is Mark's second transaction, after the unlink. The reopen runs first
// (it needs word_timing_state to still read 'queued'); then the state describing
// the removed file goes: word verdict, upgrade trip, and the stamp columns
// purge-provenance also resets. failure_class is left to migration 067's
// trigger, which clears it when the reopen blanks last_error.
func (s *Service) reopen(ctx context.Context, id int64, res *MarkResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lyricblock: begin reopen: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	reopened, err := queue.ReopenDoneRowTx(ctx, tx, id, time.Now())
	if err != nil {
		return err
	}
	if err := guardedUpdate(ctx, tx, id, clearSettledState); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lyricblock: commit reopen: %w", err)
	}
	res.Reopened = reopened
	return nil
}

// The guarded updates never touch a row a worker holds or an operator
// hand-marked.
const (
	guard          = ` WHERE id = ? AND status <> 'processing' AND manual_instrumental_at IS NULL`
	clearEditMarks = `UPDATE work_queue SET lyric_offset_ms = NULL, lyric_edited_at = NULL, sync_tier = NULL` + guard
	// clearSettledState is the word/upgrade state of the removed file plus the
	// stamp columns purge-provenance also resets.
	clearSettledState = `UPDATE work_queue SET word_timing_state = NULL, word_timing_generation = NULL, word_timing_checked_at = NULL,
		upgrade_checked_at = NULL, upgrade_queued = 0, upgrade_miss_count = 0,
		timing_stamp_source = NULL, missync_recheck_generation = NULL` + guard
)

// guardedUpdate runs query (one of the consts above) on row id and says why when
// it changed nothing. One connection: it reads through tx.
func guardedUpdate(ctx context.Context, tx *sql.Tx, id int64, query string) error {
	r, err := tx.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("lyricblock: update work item %d: %w", id, err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return fmt.Errorf("lyricblock: update work item %d rows affected: %w", id, err)
	}
	if n > 0 {
		return nil
	}
	var manual bool
	switch qerr := tx.QueryRowContext(ctx, `SELECT manual_instrumental_at IS NOT NULL FROM work_queue WHERE id = ?`, id).Scan(&manual); {
	case errors.Is(qerr, sql.ErrNoRows):
		return ErrNotFound
	case qerr != nil:
		return fmt.Errorf("lyricblock: re-read work item %d: %w", id, qerr)
	case manual:
		return ErrManualInstrumental
	}
	return ErrBusy
}

// anyBlocked reports whether t's identity already has a block.
// newFingerprints returns the fingerprints in recs that are not in known, deduplicated.
func newFingerprints(recs []Backup, known []string) []string {
	var out []string
	for _, r := range recs {
		if fp := Fingerprint(string(r.Content)); fp != "" && !slices.Contains(known, fp) && !slices.Contains(out, fp) {
			out = append(out, fp)
		}
	}
	return out
}

func (s *Service) anyBlocked(ctx context.Context, t target) (bool, error) {
	bs, err := s.store.List(ctx, ListFilter{ArtistKey: t.key[0], TitleKey: t.key[1]})
	return len(bs) > 0, err
}

func (s *Service) load(ctx context.Context, id int64) (target, error) {
	var t target
	var outdir, filename string
	var outputPaths, markedAt, editedAt, tier sql.NullString
	var offset sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT artist, title, artist_key, title_key, status, COALESCE(provider_lane, ''), COALESCE(upstream, ''), outdir, filename, output_paths,
                manual_instrumental_at, lyric_edited_at, lyric_offset_ms, sync_tier
           FROM work_queue WHERE id = ?`, id).
		Scan(&t.artist, &t.title, &t.key[0], &t.key[1], &t.status, &t.lane, &t.upstream, &outdir, &filename, &outputPaths, &markedAt, &editedAt, &offset, &tier)
	if errors.Is(err, sql.ErrNoRows) {
		return target{}, fmt.Errorf("work item %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return target{}, fmt.Errorf("lyricblock: load work item %d: %w", id, err)
	}
	t.manual = markedAt.Valid
	t.meta = map[string]string{}
	if editedAt.Valid {
		t.meta["lyric_edited_at"] = editedAt.String
	}
	if offset.Valid {
		t.meta["lyric_offset_ms"] = strconv.FormatInt(offset.Int64, 10)
	}
	if tier.Valid {
		t.meta["sync_tier"] = tier.String
	}
	if outputPaths.Valid && outputPaths.String != "" {
		if err := json.Unmarshal([]byte(outputPaths.String), &t.outputs); err != nil {
			return target{}, fmt.Errorf("lyricblock: work item %d output_paths: %w", id, err)
		}
	}
	if len(t.outputs) == 0 {
		t.outputs = []models.OutputPath{{Outdir: outdir, Filename: filename}}
	}
	t.cacheKeys = [][2]string{{t.artist, t.title}}
	seen := map[[2]string]bool{{normalize.NormalizeKey(t.artist), normalize.NormalizeKey(t.title)}: true}
	rows, err := s.db.QueryContext(ctx,
		`SELECT sr.artist, sr.title FROM scan_results sr
           JOIN work_queue_scan_results j ON j.scan_result_id = sr.id WHERE j.work_queue_id = ? ORDER BY sr.id`, id)
	if err != nil {
		return target{}, fmt.Errorf("lyricblock: links for work item %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a, ti string
		if err := rows.Scan(&a, &ti); err != nil {
			return target{}, fmt.Errorf("lyricblock: scan link: %w", err)
		}
		nk := [2]string{normalize.NormalizeKey(a), normalize.NormalizeKey(ti)}
		if !seen[nk] {
			seen[nk] = true
			t.cacheKeys = append(t.cacheKeys, [2]string{a, ti})
		}
	}
	if err := rows.Err(); err != nil {
		return target{}, fmt.Errorf("lyricblock: links rows: %w", err)
	}
	return t, nil
}
