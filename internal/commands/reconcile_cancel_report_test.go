package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/queue"
)

// cancelAfterFirstStamp applies the first row (writing its backup record) and
// cancels the run's context, so the run stops at the next row boundary with one
// row changed.
type cancelAfterFirstStamp struct {
	cancel context.CancelFunc
	calls  int
}

func (f *cancelAfterFirstStamp) SetSyncTierIfPending(_ context.Context, _ int64, _ string, backup func() error) (bool, error) {
	f.calls++
	if err := backup(); err != nil {
		return false, err
	}
	f.cancel()
	return true, nil
}

// TestRunReconcileSyncTierCore_CanceledRunStillReportsItsBackup (#1612
// review): a run canceled after a row changed exits 1 but still prints the
// counts and the backup path, so the operator can find the records to restore.
func TestRunReconcileSyncTierCore_CanceledRunStillReportsItsBackup(t *testing.T) {
	dir := t.TempDir()
	var candidates []queue.SyncTierCandidate
	for i, name := range []string{"one", "two", "three"} {
		if err := os.WriteFile(filepath.Join(dir, name+".lrc"), []byte("[00:01.00]alpha\n[00:02.00]beta\n"), 0o600); err != nil {
			t.Fatalf("write lrc: %v", err)
		}
		candidates = append(candidates, queue.SyncTierCandidate{ID: int64(i + 1), AudioPath: filepath.Join(dir, name+".flac")})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &cancelAfterFirstStamp{cancel: cancel}
	backup := filepath.Join(dir, "backup.jsonl")

	var buf bytes.Buffer
	code := runReconcileSyncTierCore(ctx, &buf, q, candidates, true, backup)
	out := buf.String()
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a canceled run; out=%s", code, out)
	}
	if q.calls != 1 {
		t.Fatalf("stamper called %d times, want 1 (the run stops at the next row)", q.calls)
	}
	if !strings.Contains(out, "reconcile-sync-tier: scanned 3 row(s)") {
		t.Errorf("canceled run printed no counts: %q", out)
	}
	if !strings.Contains(out, "backup of classified rows written to "+backup) {
		t.Errorf("canceled run did not name its backup file: %q", out)
	}
}

// cancelAfterErrs is a context whose Err turns to Canceled after n calls, so a
// sweep over n lands the cancel at each point of a real run in turn.
type cancelAfterErrs struct {
	context.Context
	n, seen atomic.Int64
	done    chan struct{}
	closed  atomic.Bool
}

func newCancelAfterErrs(n int64) *cancelAfterErrs {
	c := &cancelAfterErrs{Context: context.Background(), done: make(chan struct{})}
	c.n.Store(n)
	return c
}

func (c *cancelAfterErrs) trip() bool {
	if c.seen.Add(1) > c.n.Load() {
		if c.closed.CompareAndSwap(false, true) {
			close(c.done)
		}
		return true
	}
	return c.closed.Load()
}

func (c *cancelAfterErrs) Err() error {
	if c.trip() {
		return context.Canceled
	}
	return nil
}

func (c *cancelAfterErrs) Done() <-chan struct{} { return c.done }

func (c *cancelAfterErrs) Deadline() (time.Time, bool) { return time.Time{}, false }

// TestRunReconcileRemediated_CanceledRunStillReportsItsBackup (#1612 review):
// whenever a canceled apply has changed a row, it exits 1 and still prints the
// counts and the backup path.
func TestRunReconcileRemediated_CanceledRunStillReportsItsBackup(t *testing.T) {
	reported := 0
	for n := int64(1); n <= 200; n++ {
		_, cfgPath, _, dir, _ := seedRemediated(t)
		backup := filepath.Join(dir, "backup.jsonl")
		var buf bytes.Buffer
		code := runReconcileRemediated(newCancelAfterErrs(n), &buf, ScanReconcileRemediatedCmd{ConfigPath: cfgPath, Yes: true, Backup: backup})
		out := buf.String()
		info, err := os.Stat(backup)
		changed := err == nil && info.Size() > 0
		if code == 0 {
			break // the cancel point is past the end of the run
		}
		if !changed {
			continue // canceled before any row changed
		}
		reported++
		if !strings.Contains(out, "reconcile-remediated: scanned") {
			t.Fatalf("n=%d: a canceled run that changed rows printed no counts: %q", n, out)
		}
		if !strings.Contains(out, "written to "+backup) {
			t.Fatalf("n=%d: a canceled run that changed rows did not name its backup file: %q", n, out)
		}
	}
	if reported == 0 {
		t.Fatal("no cancel point in the sweep stopped the run after a row changed")
	}
}
