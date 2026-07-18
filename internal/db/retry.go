package db

import (
	"context"
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
		if !IsSQLiteBusy(err) {
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
