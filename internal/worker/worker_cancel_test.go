package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
)

// A shutdown cancellation is not the item's verdict. Worker.run already handles
// cancellation correctly at the LOOP level, but the item in flight when the
// signal arrives was still completed through the ordinary failure path -- so a
// routine restart stamped 'context canceled' onto a work_queue row and left it
// in the failed bucket, indistinguishable on the Failure Analysis report from a
// genuine hard error (#733).
//
// Releasing restores the row's prior status and clears the error, so the item is
// picked up again next run with no residue and no attempt consumed.
func TestFailReleasesOnShutdownCancellation(t *testing.T) {
	q := &fakeQueue{}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	item := queue.WorkItem{ID: 7}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the shutdown signal has already arrived

	if err := w.fail(ctx, item, fmt.Errorf("worker: find lyrics: %w", context.Canceled)); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if len(q.failed) != 0 {
		t.Errorf("item was marked failed on a shutdown cancellation; a restart must leave no residue on the failures view")
	}
	if len(q.released) != 1 || q.released[0] != item.ID {
		t.Errorf("released = %v, want [%d]: the item must return to the queue for the next run", q.released, item.ID)
	}
	// The consecutive-failure counter drives the worker's backoff. A shutdown is
	// not evidence the pipeline is unhealthy, so it must not count toward it --
	// otherwise a few restarts make the next run start in a backoff it did not earn.
	if w.consecutiveFailures != 0 {
		t.Errorf("consecutiveFailures = %d, want 0 (a shutdown is not a pipeline failure)", w.consecutiveFailures)
	}
}

// A DeadlineExceeded from a per-request timeout is a REAL failure and must keep
// being recorded as one. The discriminator is whether the parent context was
// canceled, not the error value alone -- without that, this fix would silently
// swallow every provider timeout, which is a far worse bug than the cosmetic one
// it set out to fix.
func TestFailStillFailsOnRequestTimeout(t *testing.T) {
	q := &fakeQueue{}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	item := queue.WorkItem{ID: 8}

	// Live parent context: the deadline belonged to one request, not to shutdown.
	cause := fmt.Errorf("musixmatch: fetch: %w", context.DeadlineExceeded)
	if err := w.fail(context.Background(), item, cause); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if len(q.failed) != 1 || q.failed[0] != item.ID {
		t.Errorf("failed = %v, want [%d]: a request timeout is a genuine failure", q.failed, item.ID)
	}
	if len(q.released) != 0 {
		t.Errorf("released = %v, want empty: a request timeout must not be released as if it were a shutdown", q.released)
	}
	if w.consecutiveFailures != 1 {
		t.Errorf("consecutiveFailures = %d, want 1 (a real failure still counts)", w.consecutiveFailures)
	}
}

// An ordinary provider error under a live context is untouched by this change.
// The regression that matters most is the one where every failure starts being
// released, silently emptying the failed bucket.
func TestFailStillFailsOnOrdinaryError(t *testing.T) {
	q := &fakeQueue{}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	item := queue.WorkItem{ID: 9}

	if err := w.fail(context.Background(), item, errors.New("musixmatch: unexpected matcher status_code 500")); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if len(q.failed) != 1 {
		t.Errorf("failed = %v, want one entry: an ordinary provider error is still a failure", q.failed)
	}
	if len(q.released) != 0 {
		t.Errorf("released = %v, want empty", q.released)
	}
}

// A canceled context with a NON-cancellation cause is still a real failure.
// Guards against keying the decision on ctx.Err() alone: an item that genuinely
// failed just before the shutdown signal arrived would otherwise have its real
// error discarded and be silently released.
func TestFailRecordsARealErrorThatRacedWithShutdown(t *testing.T) {
	q := &fakeQueue{}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	item := queue.WorkItem{ID: 10}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := w.fail(ctx, item, errors.New("worker: write item 10: refusing to write: output dir does not exist")); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if len(q.failed) != 1 {
		t.Errorf("failed = %v, want one entry: the cause is a real error, not the cancellation", q.failed)
	}
	if len(q.released) != 0 {
		t.Errorf("released = %v, want empty: a genuine failure racing a shutdown must still be recorded", q.released)
	}
}
