package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/db"
)

// Word-timing re-examination states (#982), stored in work_queue.word_timing_state.
// NULL (WorkItem.WordTimingState == "") means NOT EXAMINED, never "no words".
//
// word_timing_checked_at carries TWO meanings depending on the state it sits
// beside. On a 'served'/'absent' row it is the ordinary "verdict last reached
// at" timestamp SetWordTimingState/SettleWordRecheck stamp. On a row whose
// word_timing_state is NULL, it instead means "the sweep's flip last touched
// this row at" -- a StampChecked flip (#1048) stamps it even though the row
// has no verdict yet, specifically so a row that comes back unverdicted (the
// worker's wait-budget un-flip, or a scan reopen) is held out of the candidate
// set by UnexaminedCheckedBefore rather than re-admitted every cycle. A NULL
// row with a NULL checked_at was truly never examined; a NULL row WITH a
// checked_at was examined once and is cooling down.
const (
	// WordTimingQueued marks a settled row flipped back by MarkWordRecheckQueued.
	WordTimingQueued = "queued"
	// WordTimingServed records that a word-capable lane returned word timings.
	WordTimingServed = "served"
	// WordTimingAbsent is the terminal "no word data" marker, valid only under
	// the word_timing_generation it was stamped with.
	WordTimingAbsent = "absent"
)

// notWordRecheckQueued excludes a flipped row from every sweep that treats a
// 'deferred' row as a provider miss (instrumental backfill/recalib, timing
// sweep, library/webhook cancel): it is a settled synced row whose .lrc is
// on disk, parked for the worker only. Dequeue deliberately does NOT use it.
const notWordRecheckQueued = ` AND COALESCE(word_timing_state, '') <> 'queued'`

// WordRecheckOptions narrows the candidate set. Optional fields only SUBTRACT
// scope, except RecheckAbsentBefore, which re-admits old 'absent' verdicts.
type WordRecheckOptions struct {
	// Generation is the caller's current word-capability generation (computed
	// in internal/providers); 'absent' under any other generation re-opens.
	Generation int64
	// CompletedBefore, when non-zero, admits rows completed STRICTLY earlier
	// (a NULL completed_at is excluded: a cutoff never widens).
	CompletedBefore time.Time
	// RecheckAbsentBefore re-admits 'absent' rows checked strictly earlier.
	RecheckAbsentBefore time.Time
	// LibraryIDs admits only rows linked to one of these libraries and to no
	// library outside them (the candidate predicate; the queued count only
	// requires a link).
	LibraryIDs []int64
	// UnexaminedCheckedBefore, when non-zero, admits an unverdicted (NULL
	// state) row only if its word_timing_checked_at is NULL or strictly
	// earlier. A NULL row carries a checked_at only when a StampChecked flip
	// admitted it and it came back without a verdict (the wait-budget un-flip,
	// or a scan reopen): the serve sweep's re-admission cooldown (#1048).
	UnexaminedCheckedBefore time.Time
	// StampChecked (flip only) also sets word_timing_checked_at to the flip
	// time, so a row that returns unverdicted is held by UnexaminedCheckedBefore
	// rather than re-admitted on the next cycle. Off for the CLI, whose backup
	// record does not carry the column -- which means a CLI-queued row that
	// comes back unverdicted carries no checked_at, so it is NOT held by the
	// cooldown: the sweep (which does set StampChecked) can re-admit it once on
	// its own next cycle, at which point ITS flip finally stamps checked_at and
	// the cooldown takes over as usual from there. One bounded, one-time
	// re-admission per such row, not the unbounded per-cycle churn the cooldown
	// exists to prevent.
	StampChecked bool
	// Limit caps ListWordRecheckCandidates when > 0; the count ignores it.
	Limit int
	// BeforeCommit (flip only, optional) runs inside the flip transaction just
	// before COMMIT when any report ran, e.g. one fsync for the batch's backup
	// records; an error rolls the batch back.
	BeforeCommit func() error
}

// wordRecheckPredicate is the ONE candidate predicate (no leading WHERE/AND)
// shared by the count, the list, and the flip's in-transaction revalidation,
// so a dry-run count can never describe a different population than the rows
// an apply flips. status='done' excludes 'processing' (the worker owns
// it) and non-settled rows (fetched anyway); timing-rejected rows are never
// re-examined (mis_synced is #1007's retime source); source_path is required
// because the sidecar is derived from it.
func wordRecheckPredicate(opts WordRecheckOptions) (string, []any) {
	var b strings.Builder
	var args []any
	b.WriteString(` status = 'done'
   AND outcome_type = 'synced'
   AND COALESCE(timing_outcome, '') NOT IN ('categorical', 'mis_synced')
   AND TRIM(COALESCE(source_path, '')) <> ''
   AND ((word_timing_state IS NULL`)
	if !opts.UnexaminedCheckedBefore.IsZero() {
		b.WriteString(` AND COALESCE(word_timing_checked_at, '') < ?`)
		args = append(args, formatTime(opts.UnexaminedCheckedBefore))
	}
	b.WriteString(`)
        OR (word_timing_state = 'absent'
            AND (word_timing_generation IS NULL OR word_timing_generation <> ?))`)
	args = append(args, opts.Generation)
	if !opts.RecheckAbsentBefore.IsZero() {
		b.WriteString(`
        OR (word_timing_state = 'absent' AND COALESCE(word_timing_checked_at, '') < ?)`)
		args = append(args, formatTime(opts.RecheckAbsentBefore))
	}
	b.WriteString(`)`)
	if !opts.CompletedBefore.IsZero() {
		b.WriteString(` AND completed_at < ?`)
		args = append(args, formatTime(opts.CompletedBefore))
	}
	lib, libArgs := wordRecheckLibraryClause(opts.LibraryIDs)
	b.WriteString(lib)
	args = append(args, libArgs...)
	if len(opts.LibraryIDs) > 0 {
		// Skip a deduplicated row also linked OUTSIDE the set: the recheck
		// rewrites every linked sidecar, so a scoped run would otherwise touch
		// a library it was not given. An unscoped run covers such rows.
		ex, exArgs := libraryLinkClause(opts.LibraryIDs, ` AND id NOT IN`, `NOT IN`)
		b.WriteString(ex)
		args = append(args, exArgs...)
	}
	return b.String(), args
}

// wordRecheckLibraryClause scopes a work_queue query to rows linked (through
// work_queue_scan_results) to one of ids; empty ids scope nothing.
func wordRecheckLibraryClause(ids []int64) (string, []any) {
	return libraryLinkClause(ids, ` AND id IN`, `IN`)
}

// libraryLinkClause renders `<head> (rows linked to a scan_result whose
// library_id <op> ids)`; empty ids render nothing.
func libraryLinkClause(ids []int64, head, op string) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return head + ` (SELECT wqsr.work_queue_id FROM work_queue_scan_results wqsr` +
		` JOIN scan_results sr ON sr.id = wqsr.scan_result_id WHERE sr.library_id ` + op + ` (?` +
		strings.Repeat(`, ?`, len(ids)-1) + `))`, args
}

// CountWordRecheckCandidates is ListWordRecheckCandidates' size, ignoring Limit.
func (q *DBQueue) CountWordRecheckCandidates(ctx context.Context, opts WordRecheckOptions) (int, error) {
	pred, args := wordRecheckPredicate(opts)
	var n int
	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue WHERE`+pred, args...).Scan(&n); err != nil { //nolint:gosec // reason: G202 -- pred is built from package-constant fragments with bound parameters only
		return 0, fmt.Errorf("queue: count word recheck candidates: %w", err)
	}
	return n, nil
}

// CountWordRecheckQueued counts rows already flipped and waiting for the worker
// (word_timing_state='queued', any status), limited to libraryIDs when set. The
// reconcile-word-sync CLI reports it beside the candidate count, since those
// rows are ahead of anything a new run adds. This is the "already queued"
// figure operators see; it deliberately includes a row prune.retireUnresolvable
// retired (status='done', word_timing_state left 'queued' so the candidate
// predicate never re-flips it, per #1039) because that row genuinely is queued
// from a CLI operator's point of view -- it simply will never drain on its own.
// A caller that needs to know how many SLOTS a cap should count against wants
// CountWordRecheckInFlight instead. Read-only.
func (q *DBQueue) CountWordRecheckQueued(ctx context.Context, libraryIDs []int64) (int, error) {
	lib, args := wordRecheckLibraryClause(libraryIDs)
	var n int
	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue WHERE word_timing_state = 'queued'`+lib, args...).Scan(&n); err != nil { //nolint:gosec // reason: G202 -- lib is a package-constant fragment with bound parameters only
		return 0, fmt.Errorf("queue: count word recheck queued: %w", err)
	}
	return n, nil
}

// CountWordRecheckInFlight counts DRAINABLE recheck rows: word_timing_state =
// 'queued' AND status <> 'done'. This excludes a row prune.retireUnresolvable
// retired for a vanished source file: retirement deliberately sets status =
// 'done' while leaving word_timing_state = 'queued' (#1039, so the candidate
// predicate -- which requires word_timing_state IS NULL or 'absent' -- never
// re-flips a row it already knows is gone), which means that row can never be
// dequeued, settled, or deferred again. Counting it toward a cap holds the
// cap's slot forever and the sweep's admissions silently drift toward zero
// over the life of a library with any churn. The sweep is this function's only
// caller; CountWordRecheckQueued (any status) keeps its existing meaning for
// the reconcile-word-sync CLI's "already queued" report. A row stranded
// 'processing' + 'queued' by a crash (pre-existing gap, plan section 8 M5)
// still counts here and still occupies a slot until something reclaims it --
// this fix narrows the leak to that pre-existing, separately-tracked case
// rather than closing every way a slot can go stale.
func (q *DBQueue) CountWordRecheckInFlight(ctx context.Context, libraryIDs []int64) (int, error) {
	lib, args := wordRecheckLibraryClause(libraryIDs)
	var n int
	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue WHERE word_timing_state = 'queued' AND status <> 'done'`+lib, args...).Scan(&n); err != nil { //nolint:gosec // reason: G202 -- lib is a package-constant fragment with bound parameters only
		return 0, fmt.Errorf("queue: count word recheck in flight: %w", err)
	}
	return n, nil
}

// ListWordRecheckCandidates returns the ids of rows eligible for word-timing
// re-examination, oldest completion first so a capped run drains in a stable
// order. Read-only, and it opens no file: the state column, not the sidecar,
// says whether a row was examined.
func (q *DBQueue) ListWordRecheckCandidates(ctx context.Context, opts WordRecheckOptions) ([]int64, error) {
	pred, args := wordRecheckPredicate(opts)
	query := `SELECT id FROM work_queue WHERE` + pred + ` ORDER BY completed_at ASC, id ASC`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	return q.queryIDs(ctx, "list word recheck candidates", query, args...)
}

// ListWordTimingAbsent returns the ids of rows whose provider path is
// exhausted under generation (#1007's selection contract): 'absent' stamped
// with exactly that generation. NULL, 'queued', 'served' and a stale-generation
// 'absent' are excluded, as is any non-synced (#1007 retimes .lrc only) or
// non-done row (absent is stamped before Complete); both match the index.
// Limit caps it when > 0. Read-only.
func (q *DBQueue) ListWordTimingAbsent(ctx context.Context, generation int64, limit int) ([]int64, error) {
	query := `SELECT id FROM work_queue WHERE outcome_type = 'synced' AND status = 'done' AND word_timing_state = 'absent'
         AND word_timing_generation = ? ORDER BY id ASC`
	args := []any{generation}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	return q.queryIDs(ctx, "list word timing absent", query, args...)
}

func (q *DBQueue) queryIDs(ctx context.Context, op, query string, args ...any) (ids []int64, retErr error) {
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("queue: %s: %w", op, err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("queue: close %s rows: %w", op, err)
		}
	}()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("queue: %s scan: %w", op, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: %s rows: %w", op, err)
	}
	return ids, nil
}

// WordRecheckPrior is a row's state immediately before MarkWordRecheckQueued
// changed it: every column the flip writes, so a backup restores it exactly.
type WordRecheckPrior struct {
	ID                   int64
	Status               string
	Priority             int
	NextAttemptAt        string
	Attempts             int
	LastError            string
	RefusedWaits         int
	WordTimingState      *string
	WordTimingGeneration *int64
}

// MarkWordRecheckQueued flips settled rows back into the queue in ONE
// transaction: deferred at PriorityMiss (purgeprovenance's repair tier, behind
// fresh work), due now, attempts and last_error cleared, state 'queued'.
// miss_count, providers_version and lyrics_cache are untouched. Each id is
// revalidated against the candidate predicate for opts (Limit ignored) in both
// the prior read and the UPDATE, so a stale list never yanks a row from the
// worker nor requeues a settled one. report (optional) gets each prior state
// INSIDE the transaction, before that row's UPDATE, so a restorable backup is
// durable before the flip commits (identityrepair's write-ahead pattern); a
// report or any other error rolls the whole batch back. Retried whole on
// SQLITE_BUSY only until report has run. Returns the priors of the rows changed.
func (q *DBQueue) MarkWordRecheckQueued(ctx context.Context, ids []int64, opts WordRecheckOptions, report func(WordRecheckPrior) error) (prior []WordRecheckPrior, err error) {
	if len(ids) == 0 {
		return nil, nil
	}
	err = db.RetryBatchTx(ctx, "word recheck flip", func() error {
		var rerr error
		prior, rerr = q.markWordRecheckQueuedOnce(ctx, ids, opts, report)
		return rerr
	})
	if err != nil {
		return nil, err
	}
	return prior, nil
}

func (q *DBQueue) markWordRecheckQueuedOnce(ctx context.Context, ids []int64, opts WordRecheckOptions, report func(WordRecheckPrior) error) (prior []WordRecheckPrior, retErr error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("queue: begin word recheck flip tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	reported := false
	// Once a backup record escaped the transaction, never retry past it.
	escape := func(err error) error {
		if reported {
			return db.NotRetryable(err)
		}
		return err
	}
	now := formatTime(q.now())
	pred, predArgs := wordRecheckPredicate(opts)
	setArgs := []any{PriorityMiss, now, WordTimingQueued}
	stamp := ``
	if opts.StampChecked {
		stamp = `, word_timing_checked_at = ?`
		setArgs = append(setArgs, now)
	}
	flipSQL := `UPDATE work_queue SET status = 'deferred', priority = ?, next_attempt_at = ?, attempts = 0, last_error = '', refused_waits = 0, word_timing_state = ?` + stamp + //nolint:gosec // reason: G202 -- pred and stamp are package-constant fragments with bound parameters only
		` WHERE id = ? AND` + pred
	for _, id := range ids {
		var (
			p     WordRecheckPrior
			state sql.NullString
			gen   sql.NullInt64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, status, priority, next_attempt_at, attempts, last_error, refused_waits, word_timing_state, word_timing_generation
             FROM work_queue WHERE id = ? AND`+pred, append([]any{id}, predArgs...)...,
		).Scan(&p.ID, &p.Status, &p.Priority, &p.NextAttemptAt, &p.Attempts, &p.LastError, &p.RefusedWaits, &state, &gen)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, escape(fmt.Errorf("queue: read word recheck prior for id %d: %w", id, err))
		}
		if state.Valid {
			p.WordTimingState = &state.String
		}
		if gen.Valid {
			p.WordTimingGeneration = &gen.Int64
		}
		if report != nil {
			reported = true
			if err := report(p); err != nil {
				return nil, db.NotRetryable(fmt.Errorf("queue: report word recheck prior for id %d: %w", id, err))
			}
		}
		flipArgs := append(append(append([]any(nil), setArgs...), id), predArgs...)
		res, err := tx.ExecContext(ctx, flipSQL, flipArgs...)
		if err != nil {
			return nil, escape(fmt.Errorf("queue: flip word recheck id %d: %w", id, err))
		}
		if err := requireAffected(res, "queue: flip word recheck"); err != nil {
			return nil, escape(fmt.Errorf("queue: flip word recheck id %d: %w", id, err))
		}
		prior = append(prior, p)
	}
	if reported && opts.BeforeCommit != nil {
		if err := opts.BeforeCommit(); err != nil {
			return nil, db.NotRetryable(fmt.Errorf("queue: word recheck flip pre-commit: %w", err))
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, escape(fmt.Errorf("queue: commit word recheck flip: %w", err))
	}
	return prior, nil
}

// SetWordTimingState stamps a row's word-timing verdict (served or absent),
// the generation it was reached under, and checkedAt (zero means now). It is
// the single verdict write site (migration 051 has no CHECK constraint), so
// any other state is refused; 'queued' is written only by the flip. Keys on id
// alone with no status guard, like SetTimingOutcome; a missing id is a no-op.
func (q *DBQueue) SetWordTimingState(ctx context.Context, id int64, state string, generation int64, checkedAt time.Time) error {
	if state != WordTimingServed && state != WordTimingAbsent {
		return fmt.Errorf("queue: set word timing state for id %d: invalid state %q", id, state)
	}
	if checkedAt.IsZero() {
		checkedAt = q.now()
	}
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET word_timing_state = ?, word_timing_generation = ?, word_timing_checked_at = ? WHERE id = ?`,
		state, generation, formatTime(checkedAt), id,
	); err != nil {
		return fmt.Errorf("queue: set word timing state for id %d: %w", id, err)
	}
	return nil
}

// ClearWordTimingState drops a served/absent verdict (and its generation and
// checked_at) from a row an ordinary completion settled without one (#982
// slice 4), so a verdict about an earlier file never outlives it. 'queued' is
// never touched (the flip's state), and a row with no verdict matches nothing,
// so the common case changes no row.
func (q *DBQueue) ClearWordTimingState(ctx context.Context, id int64) error {
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET word_timing_state = NULL, word_timing_generation = NULL, word_timing_checked_at = NULL
         WHERE id = ? AND word_timing_state IN ('served', 'absent')`, id,
	); err != nil {
		return fmt.Errorf("queue: clear word timing state for id %d: %w", id, err)
	}
	return nil
}

// wordRecheckOwned is the guard both word-recheck transitions share: the
// worker holds the row AND it is still a recheck row.
const wordRecheckOwned = ` WHERE id = ? AND status = 'processing' AND word_timing_state = 'queued'`

// SettleWordRecheck settles a word-recheck row (#982) back to 'done' with its
// verdict (served or absent) and generation in ONE statement. A verdict
// stamped apart from the settle could survive a failed settle, and the retried
// row would then run as an ORDINARY fetch, whose unsynced result replaces the
// .lrc. completed_at moves only on served (a new sidecar landed); absent
// leaves it, like the file. miss_count, attempts and lane_attempts are never
// touched. sql.ErrNoRows means the row was not a processing recheck row, or
// no longer exists (pruned or cleared while the worker held it).
func (q *DBQueue) SettleWordRecheck(ctx context.Context, id int64, state string, generation int64) error {
	if state != WordTimingServed && state != WordTimingAbsent {
		return fmt.Errorf("queue: settle word recheck id %d: invalid state %q", id, state)
	}
	now := formatTime(q.now())
	return db.RetryOnBusy(ctx, dequeueMaxAttempts, func() error {
		tx, err := q.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("queue: begin word recheck settle tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		res, err := tx.ExecContext(ctx, `UPDATE work_queue SET status = 'done', last_error = '', refused_waits = 0,
             completed_at = CASE WHEN ? = 'served' THEN ? ELSE completed_at END,
             word_timing_state = ?, word_timing_generation = ?, word_timing_checked_at = ?`+wordRecheckOwned,
			state, now, state, generation, now, id)
		if err != nil {
			return fmt.Errorf("queue: settle word recheck id %d: %w", id, err)
		}
		if err := requireAffected(res, "queue: settle word recheck"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE scan_results SET status = 'done'
             WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
               AND status != 'done'`, id); err != nil {
			return fmt.Errorf("queue: settle word recheck scan_results writeback: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("queue: commit word recheck settle: %w", err)
		}
		return nil
	})
}

// DeferWordRecheck releases a word-recheck row whose word question went
// unanswered (a lane throttled, was unavailable, or gave no usable answer) for
// a retry after retryAfter. The row stays 'queued' and nothing a miss or a
// failure counts changes (miss_count, attempts): a lane that did not answer has
// not said "no words". The delay is what a plain Release lacks: a released
// recheck row is due again at once and would be re-asked in a tight loop.
//
// The wait is bounded by maxWaits on refused_waits, the same per-row "waited
// for a lane that did not answer" budget #950 uses (every settle zeroes it, so
// a flipped row starts at 0). Once spent it returns released=true having
// UN-FLIPPED the row instead: back to 'done' with word_timing_state NULL,
// completed_at untouched, so it stops rechecking and stays a candidate for a
// later run. Never absent: an unanswered lane has not said "no words".
func (q *DBQueue) DeferWordRecheck(ctx context.Context, id int64, retryAfter time.Duration, maxWaits int, cause string) (released bool, err error) {
	// Retried like Settle: a lost write strands the row in 'processing', which
	// nothing reclaims.
	err = db.RetryOnBusy(ctx, dequeueMaxAttempts, func() error {
		// Per attempt, like deferRefusedOnce: a long busy wait must not shorten the park.
		next := formatTime(q.now().Add(retryAfter))
		tx, err := q.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("queue: begin defer word recheck tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		// Every CASE reads the PRE-update refused_waits.
		err = tx.QueryRowContext(ctx, `UPDATE work_queue SET
             status = CASE WHEN refused_waits >= ? THEN 'done' ELSE 'deferred' END,
             word_timing_state = CASE WHEN refused_waits >= ? THEN NULL ELSE word_timing_state END,
             next_attempt_at = CASE WHEN refused_waits >= ? THEN next_attempt_at ELSE ? END,
             last_error = CASE WHEN refused_waits >= ? THEN '' ELSE ? END,
             refused_waits = CASE WHEN refused_waits >= ? THEN 0 ELSE refused_waits + 1 END`+wordRecheckOwned+
			` RETURNING status = 'done'`, maxWaits, maxWaits, maxWaits, next, maxWaits, cause, maxWaits, id).Scan(&released)
		if err != nil {
			return fmt.Errorf("queue: defer word recheck id %d: %w", id, err)
		}
		if released {
			if _, err := tx.ExecContext(ctx, `UPDATE scan_results SET status = 'done'
             WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
               AND status != 'done'`, id); err != nil {
				return fmt.Errorf("queue: release word recheck scan_results writeback: %w", err)
			}
		}
		return tx.Commit()
	})
	return released, err
}
