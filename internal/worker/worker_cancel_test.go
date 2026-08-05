package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

// A failed Release must surface, not be swallowed. If the release errors the
// item is still 'processing' in the database, so silently returning nil would
// leave it wedged there with nothing reporting why -- the worker that owns it is
// shutting down and will not come back to it.
func TestFailSurfacesAReleaseFailureOnShutdown(t *testing.T) {
	q := &fakeQueue{releaseErr: errors.New("database is locked")}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	item := queue.WorkItem{ID: 11}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := w.fail(ctx, item, fmt.Errorf("worker: find lyrics: %w", context.Canceled))
	if err == nil {
		t.Fatal("a failed release returned nil; the item is left 'processing' with nothing reporting it")
	}
	if !strings.Contains(err.Error(), "release item 11") {
		t.Errorf("error does not identify the item or the operation: %v", err)
	}
	// It must not silently fall through to the failure path either -- that would
	// record a hard failure for what was only a shutdown.
	if len(q.failed) != 0 {
		t.Errorf("failed = %v, want empty: a release error must not become a recorded failure", q.failed)
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

// A failed Fail must surface too, carrying BOTH the original cause and the
// bookkeeping error. Losing the original would leave an operator with "the
// database was busy" and no trace of what the item actually did wrong.
//
// This branch predates the shutdown-release change, but it sits in the same
// function and was never exercised; covering it here is cheaper than arguing it
// out of scope.
func TestFailSurfacesABookkeepingFailure(t *testing.T) {
	q := &fakeQueue{failErr: errors.New("database is locked")}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	item := queue.WorkItem{ID: 12}

	cause := errors.New("musixmatch: unexpected matcher status_code 500")
	err := w.fail(context.Background(), item, cause)
	if err == nil {
		t.Fatal("a failed Fail returned nil; the item is left 'processing' with nothing reporting it")
	}
	if !strings.Contains(err.Error(), "status_code 500") {
		t.Errorf("the original cause was lost, leaving only the bookkeeping error: %v", err)
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("the bookkeeping error was lost: %v", err)
	}
	// The counter still moved: the item DID fail, even though recording it did not.
	if w.consecutiveFailures != 1 {
		t.Errorf("consecutiveFailures = %d, want 1", w.consecutiveFailures)
	}
}

// An EXPIRED-BY-DEADLINE parent is a timeout, not a shutdown, even when the
// cause happens to wrap context.Canceled -- a lane can surface a canceled inner
// request while the worker's own context died of a deadline.
//
// The original guard used `ctx.Err() != nil`, which admits DeadlineExceeded and
// so released the item and skipped failure accounting entirely. Verified against
// a real expired WithTimeout parent before fixing: the loose form takes the
// release branch, the strict `errors.Is(ctx.Err(), context.Canceled)` does not.
//
// No caller wraps the worker context in a WithTimeout today, so this is latent
// -- but it is one line to be exactly right, and a silently-swallowed timeout is
// the kind of bug nobody reports. Found by CodeRabbit on #736.
func TestFailStillFailsWhenParentExpiredByDeadline(t *testing.T) {
	q := &fakeQueue{}
	w := New(q, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	item := queue.WorkItem{ID: 13}

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // let the deadline actually pass

	// The cause wraps Canceled (an inner request was canceled) while the parent
	// died of a DEADLINE. That combination is what separates the two guards.
	if err := w.fail(ctx, item, fmt.Errorf("lane musixmatch: %w", context.Canceled)); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if len(q.failed) != 1 || q.failed[0] != item.ID {
		t.Errorf("failed = %v, want [%d]: a deadline is a timeout, not a shutdown", q.failed, item.ID)
	}
	if len(q.released) != 0 {
		t.Errorf("released = %v, want empty: releasing here would swallow the timeout unrecorded", q.released)
	}
	if w.consecutiveFailures != 1 {
		t.Errorf("consecutiveFailures = %d, want 1 (a timeout still counts against the pipeline)", w.consecutiveFailures)
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
