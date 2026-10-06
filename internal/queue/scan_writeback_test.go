package queue

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// readScanStatus returns the status of one scan_results row.
func readScanStatus(t *testing.T, sqlDB *sql.DB, id int64) string {
	t.Helper()
	var s string
	if err := sqlDB.QueryRow(`SELECT status FROM scan_results WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read scan_result %d: %v", id, err)
	}
	return s
}

// TestScanResultLinkedToTwoLiveRows reproduces the #1038 multi-link scenario
// through the public API: a scan result linked under one key is re-keyed by a
// forced rescan while its first row is mid-fetch, and Enqueue links it under
// the new key without removing the old link. Completing the old row must not
// mark the result done while the new row is still pending.
func TestScanResultLinkedToTwoLiveRows(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }
	srID := insertScanResult(t, sqlDB, "/music/retag.flac")

	enqueue := func(artist string) int64 {
		t.Helper()
		item, err := q.Enqueue(ctx, models.Inputs{
			Track: models.Track{ArtistName: artist, TrackName: "Song"}, ScanResultID: srID,
		}, PriorityScan)
		if err != nil {
			t.Fatalf("Enqueue %s: %v", artist, err)
		}
		return item.ID
	}
	oldID := enqueue("Old Artist")
	if got, err := q.Dequeue(ctx); err != nil || got.ID != oldID {
		t.Fatalf("Dequeue = (%d, %v); want old row %d", got.ID, err, oldID)
	}
	// The forced rescan re-pends the retagged result; the enqueuer links it
	// under its new key while the old row is still processing.
	mustExec(t, sqlDB, `UPDATE scan_results SET status = 'pending' WHERE id = ?`, srID)
	newID := enqueue("New Artist")
	var links int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM work_queue_scan_results WHERE scan_result_id = ?`, srID).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 2 || newID == oldID {
		t.Fatalf("links = %d, rows %d/%d; want one result linked to two rows", links, oldID, newID)
	}

	if err := q.Complete(ctx, oldID); err != nil {
		t.Fatalf("Complete old: %v", err)
	}
	if got := readScanStatus(t, sqlDB, srID); got != "pending" {
		t.Fatalf("after old row completed: scan_result = %q; want pending (new row %d is unfinished)", got, newID)
	}
	if got, err := q.Dequeue(ctx); err != nil || got.ID != newID {
		t.Fatalf("Dequeue = (%d, %v); want new row %d", got.ID, err, newID)
	}
	if err := q.Complete(ctx, newID); err != nil {
		t.Fatalf("Complete new: %v", err)
	}
	if got := readScanStatus(t, sqlDB, srID); got != "done" {
		t.Fatalf("after last row completed: scan_result = %q; want done", got)
	}
}

// TestDoneWritebacksHonorLiveSibling pins the shared owner guard on every
// 'done' writeback over a linked row: each path marks the result done unless
// another linked row is live, and a re-trip of a settled row (word recheck,
// upgrade trip) does not count as live.
func TestDoneWritebacksHonorLiveSibling(t *testing.T) {
	type path struct {
		shape string // SET clause applied to the settling row
		run   func(ctx context.Context, q *DBQueue, id int64) error
	}
	paths := map[string]path{
		"Complete": {"status = 'processing'", func(ctx context.Context, q *DBQueue, id int64) error {
			return q.Complete(ctx, id)
		}},
		"SettleInstrumental": {"status = 'processing'", func(ctx context.Context, q *DBQueue, id int64) error {
			_, err := q.SettleInstrumental(ctx, id, InstrumentalTelemetry{}, OwnedByWorker)
			return err
		}},
		"SettleGuardRejected": {"status = 'processing'", func(ctx context.Context, q *DBQueue, id int64) error {
			_, err := q.SettleGuardRejected(ctx, id, "script")
			return err
		}},
		"RetireMiss": {"status = 'processing'", func(ctx context.Context, q *DBQueue, id int64) error {
			_, err := q.RetireMiss(ctx, id)
			return err
		}},
		"SettleWordRecheck": {"status = 'processing', word_timing_state = 'queued'", func(ctx context.Context, q *DBQueue, id int64) error {
			return q.SettleWordRecheck(ctx, id, WordTimingServed, 1)
		}},
		"DeferWordRecheck release": {"status = 'processing', word_timing_state = 'queued', refused_waits = 1",
			func(ctx context.Context, q *DBQueue, id int64) error {
				released, err := q.DeferWordRecheck(ctx, id, time.Hour, 1, "x")
				if err == nil && !released {
					t.Errorf("DeferWordRecheck did not release")
				}
				return err
			}},
	}
	siblings := []struct{ name, shape, want string }{
		{"no sibling", "", "done"},
		{"pending sibling blocks", "status = 'pending'", "pending"},
		{"deferred sibling blocks", "status = 'deferred'", "pending"},
		{"failed sibling blocks", "status = 'failed'", "pending"},
		{"processing sibling blocks", "status = 'processing'", "pending"},
		{"done sibling", "status = 'done'", "done"},
		{"unavailable sibling", "status = 'unavailable'", "done"},
		{"word-recheck sibling does not block", "status = 'deferred', word_timing_state = 'queued'", "done"},
		{"upgrade-trip sibling does not block", "status = 'pending', upgrade_queued = 1", "done"},
	}
	for pname, p := range paths {
		for _, sib := range siblings {
			t.Run(pname+"/"+sib.name, func(t *testing.T) {
				ctx := context.Background()
				sqlDB := openQueueTestDB(t)
				q := NewDBQueue(sqlDB)
				q.now = func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) }
				srID := insertScanResult(t, sqlDB, "/music/x.flac")
				mustExec(t, sqlDB, `UPDATE scan_results SET status = 'pending' WHERE id = ?`, srID)
				seed := func(key, shape string) int64 {
					id := seedWordCandidate(t, sqlDB, key)
					mustExec(t, sqlDB, `UPDATE work_queue SET `+shape+` WHERE id = ?`, id) //nolint:gosec // reason: G202: shape is a test-authored literal
					mustExec(t, sqlDB, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, srID)
					return id
				}
				id := seed("self", p.shape)
				if sib.shape != "" {
					seed("sibling", sib.shape)
				}
				if err := p.run(ctx, q, id); err != nil {
					t.Fatalf("%s: %v", pname, err)
				}
				if got := readScanStatus(t, sqlDB, srID); got != sib.want {
					t.Fatalf("scan_result = %q; want %q", got, sib.want)
				}
			})
		}
	}
}
