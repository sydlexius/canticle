// Package wordsyncrepair holds the selection and queuing of `scan
// reconcile-word-sync` (#982): count the settled line-synced rows due a
// word-timing re-check, then (when applying) flip them back into the queue in
// bounded write-ahead-backed batches. It never fetches; a running serve worker
// drains the rows. The package takes a queue (an interface) and a Backup
// instead of a config path, never opens a database and never prints; the
// command keeps flag parsing, config loading, the database open and the
// aggregate stdout.
//
// Library paths, artists, titles and lyric lines never reach a count, a log
// field or an error. The one path an error may name is the operator's own
// backup file (a failed backup open).
package wordsyncrepair

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/sydlexius/canticle/internal/queue"
)

// FlipBatch bounds one flip transaction, which holds the writer lock, so a
// library-sized run in one transaction would stall a live serve.
const FlipBatch = 500

// ErrNoBackup reports an applying run given no Backup. A run that changes rows
// without a backup is refused, never silently unbacked.
var ErrNoBackup = errors.New("wordsyncrepair: an applying run needs a backup")

// Queue is the queue surface the repair needs. *queue.DBQueue satisfies it.
type Queue interface {
	CountWordRecheckCandidates(ctx context.Context, opts queue.WordRecheckOptions) (int, error)
	CountWordRecheckQueued(ctx context.Context, libraryIDs []int64) (int, error)
	ListWordRecheckCandidates(ctx context.Context, opts queue.WordRecheckOptions) ([]int64, error)
	MarkWordRecheckQueued(ctx context.Context, ids []int64, opts queue.WordRecheckOptions, report func(queue.WordRecheckPrior) error) ([]queue.WordRecheckPrior, error)
}

// Backup receives each flipped row's prior state. Record runs INSIDE the flip
// transaction, before that row's UPDATE, so it must not touch the database
// (the handle has one connection, which the flip holds). Sync runs once per
// batch just before COMMIT (write-ahead). An error from either rolls the batch
// back.
type Backup interface {
	Record(p queue.WordRecheckPrior) error
	Sync() error
}

// Options control one run. A dry run (Apply false) only counts and ignores
// Backup.
type Options struct {
	Apply bool
	// Query carries the cutoffs, Limit, library scope and word generation.
	// BeforeCommit is overwritten by Run.
	Query    queue.WordRecheckOptions
	Backup   Backup
	Progress func(done, total int)
}

// Result is a run's counts. After a failed or canceled run it holds what was
// done so far.
type Result struct {
	Candidates    int
	Selected      int // Candidates, capped by Query.Limit
	AlreadyQueued int
	Queued        int // rows flipped (committed batches only)
	Written       int // backup records written, including a rolled-back batch's
}

// StageError names the stage of Run that failed: "count candidates", "count
// queued rows", "list candidates" or "flip". A flip failure rolled its batch
// back.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return "wordsyncrepair: " + e.Stage + ": " + e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

// Run counts the candidates and, when o.Apply, flips the selected rows in
// batches of FlipBatch. When ctx is canceled it stops at the next batch
// boundary and returns the partial result with an error wrapping ctx.Err();
// committed batches stay committed and no batch is half-applied.
func Run(ctx context.Context, q Queue, o Options) (Result, error) {
	var res Result
	if o.Apply && o.Backup == nil {
		return res, ErrNoBackup
	}
	opts := o.Query
	opts.BeforeCommit = nil
	var err error
	if res.Candidates, err = q.CountWordRecheckCandidates(ctx, opts); err != nil {
		return res, &StageError{Stage: "count candidates", Err: err}
	}
	if res.AlreadyQueued, err = q.CountWordRecheckQueued(ctx, opts.LibraryIDs); err != nil {
		return res, &StageError{Stage: "count queued rows", Err: err}
	}
	res.Selected = res.Candidates
	if opts.Limit > 0 && opts.Limit < res.Selected {
		res.Selected = opts.Limit
	}
	if !o.Apply || res.Selected == 0 {
		return res, nil
	}

	report := func(p queue.WordRecheckPrior) error {
		if err := o.Backup.Record(p); err != nil {
			return err //nolint:wrapcheck // reason: the Backup's own error, already carrying its context
		}
		res.Written++
		return nil
	}
	opts.BeforeCommit = o.Backup.Sync
	ids, err := q.ListWordRecheckCandidates(ctx, opts)
	if err != nil {
		return res, &StageError{Stage: "list candidates", Err: err}
	}
	for start := 0; start < len(ids); start += FlipBatch {
		if cerr := ctx.Err(); cerr != nil {
			return res, fmt.Errorf("word-sync repair stopped: %w", cerr)
		}
		end := min(start+FlipBatch, len(ids))
		prior, ferr := q.MarkWordRecheckQueued(ctx, ids[start:end], opts, report)
		if ferr != nil {
			return res, &StageError{Stage: "flip", Err: ferr}
		}
		res.Queued += len(prior)
		if o.Progress != nil {
			o.Progress(end, len(ids))
		}
	}
	return res, nil
}

// BackupRecord is one JSONL line: a flipped row's prior value of every column
// the flip writes (nil = NULL), so writing it back restores the row.
type BackupRecord struct {
	ID                   int64   `json:"id"`
	Status               string  `json:"status"`
	Priority             int     `json:"priority"`
	NextAttemptAt        string  `json:"next_attempt_at"`
	Attempts             int     `json:"attempts"`
	LastError            string  `json:"last_error"`
	RefusedWaits         int     `json:"refused_waits"`
	WordTimingState      *string `json:"word_timing_state"`
	WordTimingGeneration *int64  `json:"word_timing_generation"`
}

// AppendRecord writes p to w as one JSONL line.
func AppendRecord(w io.Writer, p queue.WordRecheckPrior) error {
	b, err := json.Marshal(BackupRecord{
		ID: p.ID, Status: p.Status, Priority: p.Priority, NextAttemptAt: p.NextAttemptAt,
		Attempts: p.Attempts, LastError: p.LastError, RefusedWaits: p.RefusedWaits,
		WordTimingState: p.WordTimingState, WordTimingGeneration: p.WordTimingGeneration,
	})
	if err != nil {
		return fmt.Errorf("marshal reconcile-word-sync backup record: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write reconcile-word-sync backup record: %w", err)
	}
	return nil
}
