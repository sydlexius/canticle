package db

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sydlexius/canticle/internal/backoff"
)

// Short backoff bounds for SQLITE_BUSY retries. busy_timeout already waits up to
// 5s per attempt, so these only space out the rare retry that follows a timeout.
const (
	retryBaseDelay = 100 * time.Millisecond
	retryMaxDelay  = 2 * time.Second
)

// BatchTxAttempts is the attempt budget for one unit of work (typically one
// row's transaction) in a batch mutator that may run against a live serve
// (#978). Each attempt can wait up to busy_timeout (5s) at BEGIN IMMEDIATE,
// plus a backoff capped at retryMaxDelay, so 20 attempts is roughly two and a
// half minutes of patience per unit before the batch gives up.
const BatchTxAttempts = 20

// batchTxWarnAfter is the attempt count past which RetryBatchTx logs at Warn:
// an occasional single retry is routine contention, several is worth seeing.
const batchTxWarnAfter = 3

// RetryBatchTx is RetryOnBusy with the BatchTxAttempts budget, logging at Warn
// when op needed more than a few attempts. fn must be one whole transaction
// (begin through commit) so a retry re-runs it from a clean slate; any side
// effect fn has already made OUTSIDE the database before failing (for example a
// backup record) must make its error non-retryable with NotRetryable.
func RetryBatchTx(ctx context.Context, op string, fn func() error) error {
	attempts := 0
	err := RetryOnBusy(ctx, BatchTxAttempts, func() error {
		attempts++
		return fn()
	})
	if attempts > batchTxWarnAfter {
		slog.Warn("db: transaction needed retries after SQLITE_BUSY", "op", op, "attempts", attempts, "error", err)
	}
	return err
}

// notRetryableError marks an error RetryOnBusy must return rather than retry.
// It keeps the wrap chain, so errors.Is/As (and IsSQLiteBusy) still see through.
type notRetryableError struct{ err error }

func (e notRetryableError) Error() string { return e.err.Error() }
func (e notRetryableError) Unwrap() error { return e.err }

// NotRetryable marks err so RetryOnBusy returns it immediately even when it is a
// SQLITE_BUSY. A nil err stays nil.
func NotRetryable(err error) error {
	if err == nil {
		return nil
	}
	return notRetryableError{err: err}
}

// RetryOnBusy calls fn up to maxAttempts times, retrying only on SQLITE_BUSY
// with geometric backoff between attempts. It returns nil on the first success,
// any non-SQLITE_BUSY error immediately, or the last SQLITE_BUSY error once
// attempts are exhausted. Backoff sleeps honor ctx cancellation.
func RetryOnBusy(ctx context.Context, maxAttempts int, fn func() error) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		var nr notRetryableError
		if !IsSQLiteBusy(err) || errors.As(err, &nr) {
			return err
		}
		if attempt == maxAttempts-1 {
			break
		}
		delay := backoff.Geometric(attempt+1, retryBaseDelay, retryMaxDelay)
		slog.Debug("retrying after SQLITE_BUSY", "attempt", attempt+1, "delay", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return err
}
