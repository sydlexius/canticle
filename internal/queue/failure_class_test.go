package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

// classOf reads a row's failure_class ("" for NULL) and status.
func classOf(t *testing.T, q *DBQueue, id int64) (class, status string) {
	t.Helper()
	if err := q.db.QueryRowContext(context.Background(),
		`SELECT COALESCE(failure_class, ''), status FROM work_queue WHERE id = ?`, id).Scan(&class, &status); err != nil {
		t.Fatalf("read row %d: %v", id, err)
	}
	return class, status
}

func errOf[T any](_ T, err error) error { return err }

type classStep = func(q *DBQueue, id int64) error

// Every writer of a failure stamps its class in the statement that writes
// last_error (#1285). Each case starts from one claimed row holding a stale
// class, so a writer that does not assign the column is caught.
func TestFailureWritersStampTheClass(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("lane a: boom")
	for _, tc := range []struct {
		name       string
		write      classStep
		wantClass  string
		wantStatus string
	}{
		{"Fail, classed cause", func(q *DBQueue, id int64) error { return errOf(q.Fail(ctx, id, WithFailureClass(boom, FailureWrite))) }, "write", StatusFailed},
		{"Fail, unclassed cause", func(q *DBQueue, id int64) error { return errOf(q.Fail(ctx, id, boom)) }, "", StatusFailed},
		{"Fail, blank message", func(q *DBQueue, id int64) error {
			return errOf(q.Fail(ctx, id, WithFailureClass(errors.New(" \t"), FailureNetwork)))
		}, "none", StatusFailed},
		{"Defer", func(q *DBQueue, id int64) error {
			return errOf(q.Defer(ctx, id, time.Hour, WithFailureClass(boom, FailureMiss)))
		}, "miss", StatusDeferred},
		{"DeferRefused", func(q *DBQueue, id int64) error {
			return errOf(q.DeferRefused(ctx, id, time.Hour, 3, "lane a did not answer"))
		}, "throttle", StatusDeferred},
		{"RetireMiss", func(q *DBQueue, id int64) error { return errOf(q.RetireMiss(ctx, id)) }, "miss", StatusUnavailable},
		{"DeferWordRecheck", func(q *DBQueue, id int64) error {
			return errOf(q.DeferWordRecheck(ctx, id, time.Hour, 3, "lane a: 502"))
		}, "throttle", StatusDeferred},
		{"DeferWordRecheck, classed", func(q *DBQueue, id int64) error {
			return errOf(q.DeferWordRecheck(ctx, id, time.Hour, 3, "x", FailureMiss))
		}, "miss", StatusDeferred},
		{"RetryWordRecheckWrite", func(q *DBQueue, id int64) error { return q.RetryWordRecheckWrite(ctx, id, time.Hour, "stamp failed") }, "write", StatusDeferred},
		{"UnsettleInstrumental", func(q *DBQueue, id int64) error {
			mustExec(t, q.db, `UPDATE work_queue SET status = 'done', instrumental_result = 1, failure_class = NULL WHERE id = ?`, id)
			return errOf(q.UnsettleInstrumental(ctx, id))
		}, "other", StatusDeferred},
	} {
		q, id, _ := newRefusedQueue(t, 0)
		mustExec(t, q.db, `UPDATE work_queue SET failure_class = 'stale', word_timing_state = 'queued' WHERE id = ?`, id)
		if err := tc.write(q, id); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if class, status := classOf(t, q, id); class != tc.wantClass || status != tc.wantStatus {
			t.Errorf("%s: failure_class = %q, status %q; want %q, %q", tc.name, class, status, tc.wantClass, tc.wantStatus)
		}
	}
}

// A row that leaves the failure states, or whose last_error is cleared, drops
// its class; a claim and a release back to the failure state keep it.
func TestFailureClassClearsWhenTheFailureDoes(t *testing.T) {
	ctx := context.Background()
	claim := func(q *DBQueue, _ int64) error { return errOf(q.Dequeue(ctx)) }
	sql := func(stmt string, args ...any) classStep {
		return func(q *DBQueue, id int64) error { return errOf(q.db.ExecContext(ctx, stmt, append(args, id)...)) }
	}
	for _, tc := range []struct {
		name       string
		seed       string // status the classed row starts in
		steps      []classStep
		wantClass  string
		wantStatus string
	}{
		{"claimed and released", StatusDeferred, []classStep{claim, func(q *DBQueue, id int64) error { return q.Release(ctx, id) }}, "miss", StatusDeferred},
		{"completed", StatusFailed, []classStep{claim, func(q *DBQueue, id int64) error { return q.Complete(ctx, id) }}, "", StatusDone},
		{"retried by hand", StatusFailed, []classStep{func(q *DBQueue, id int64) error { return errOf(q.Retry(ctx, id)) }}, "", StatusPending},
		{"given up, then rechecked", StatusUnavailable, []classStep{
			sql(`UPDATE work_queue SET last_error = ? WHERE id = ?`, missLimitReachedError),
			func(q *DBQueue, _ int64) error { return errOf(q.RecheckRetired(ctx, nil)) },
		}, "", StatusDeferred},
		{"word recheck released to done", StatusDeferred, []classStep{claim,
			sql(`UPDATE work_queue SET word_timing_state = 'queued' WHERE id = ?`),
			func(q *DBQueue, id int64) error {
				return errOf(q.DeferWordRecheck(ctx, id, time.Hour, 0, "lane a: 502"))
			},
		}, "", StatusDone},
		// Shapes with no '' to key on: prune's retire, a release to pending.
		{"retired to done with a sentinel", StatusFailed, []classStep{
			sql(`UPDATE work_queue SET status = 'done', last_error = ? WHERE id = ?`, UnresolvableGoneError)}, "", StatusDone},
		{"back to pending, message kept", StatusFailed, []classStep{
			sql(`UPDATE work_queue SET status = 'pending', last_error = 'cause cleared by release' WHERE id = ?`)}, "", StatusPending},
	} {
		q, id, _ := newRefusedQueue(t, 0)
		mustExec(t, q.db, `UPDATE work_queue SET status = ?, last_error = 'lane a: no results found', failure_class = 'miss',
            next_attempt_at = '2000-01-01T00:00:00Z' WHERE id = ?`, tc.seed, id)
		for _, step := range tc.steps {
			if err := step(q, id); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
		if class, status := classOf(t, q, id); class != tc.wantClass || status != tc.wantStatus {
			t.Errorf("%s: failure_class = %q, status %q; want %q, %q", tc.name, class, status, tc.wantClass, tc.wantStatus)
		}
	}
}
