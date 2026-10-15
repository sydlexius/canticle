package commands

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// tripCtx is canceled the nth time Done is consulted, so a sweep over n lands
// the cancel on every database call the command makes.
type tripCtx struct {
	context.Context
	n     int32
	calls atomic.Int32
	once  sync.Once
	done  chan struct{}
}

func newTripCtx(parent context.Context, n int) *tripCtx {
	return &tripCtx{Context: parent, n: int32(n), done: make(chan struct{})} //nolint:gosec // reason: n is a small test sweep index
}

func (c *tripCtx) Done() <-chan struct{} {
	if c.calls.Add(1) >= c.n {
		c.once.Do(func() { close(c.done) })
	}
	return c.done
}

func (c *tripCtx) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

// A dry run applies nothing, so no batch exists to roll back: a cancel that
// lands on a count or list stage must report "<stage> failed" on the log like
// the pre-extraction command did, and never print the rolled-back line.
func TestReconcileWordSync_CancelDuringCountsPrintsNoRollbackLine(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	stageFailed := 0
	for n := 1; n <= 80; n++ {
		ctx, cfgPath, _ := setupWordSync(t, "")
		logs.Reset()
		code, out := runWS(t, newTripCtx(ctx, n), ScanReconcileWordSyncCmd{ConfigPath: cfgPath})
		if code == 0 {
			continue
		}
		if strings.Contains(out, "rolled back") || strings.Contains(out, "stopped after queuing") {
			t.Fatalf("n=%d: dry-run cancel printed a rollback line: %q", n, out)
		}
		l := logs.String()
		if strings.Contains(l, "count candidates failed") || strings.Contains(l, "count queued rows failed") || strings.Contains(l, "list candidates failed") {
			stageFailed++
		}
		if strings.Contains(l, "flip failed") {
			t.Fatalf("n=%d: a count-stage cancel was logged as a flip failure: %s", n, l)
		}
	}
	if stageFailed == 0 {
		t.Fatal("no cancel landed on a count stage; the sweep proves nothing")
	}
}
