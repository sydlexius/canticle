package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
)

// TestScanRepairsRowAfterInPlaceFormatSwap is #1262 end to end over real files
// and real SQLite: a track's .mp3 is scanned and its row settles, the album is
// re-ripped in place to .flac, and the next ordinary scan must leave the one
// row naming the .flac with its settle record intact, with no reconcile run.
//
// The two cases reach the repair by different routes. With no sidecar the
// .flac is pending and collides in Enqueue. With a surviving .lrc the scanner
// indexes the .flac as already settled, so it is never enqueued and only
// RepointSettled sees it.
func TestScanRepairsRowAfterInPlaceFormatSwap(t *testing.T) {
	tests := []struct {
		name       string
		sidecar    bool
		wantStatus string
		wantEnq    int
	}{
		{"retired miss, no sidecar: repaired by the enqueue collision", false, queue.StatusUnavailable, 1},
		{"completed with a sidecar: repaired by RepointSettled", true, queue.StatusDone, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sqlDB := openTestDB(t)
			repo := scan.New(sqlDB)
			lib, err := library.New(sqlDB).Add(ctx, t.TempDir(), "Music", models.LibrarySettings{})
			if err != nil {
				t.Fatalf("Add library: %v", err)
			}
			dir := lib.Path
			sc := scanner.NewScanner(scanner.WithIndexStore(repo))
			q := queue.NewDBQueue(sqlDB)
			enq := scan.Enqueuer{Results: repo, Cache: cache.New(sqlDB), Queue: q, Priority: queue.PriorityScan}
			// rescan mirrors the scheduler: scan, upsert, then the completion hook's
			// repoint and enqueue.
			rescan := func() (moved, enqueued int) {
				t.Helper()
				results, err := sc.ScanLibrary(ctx, dir, scanner.ScanOptions{MaxDepth: 1})
				if err != nil {
					t.Fatalf("ScanLibrary: %v", err)
				}
				for i := range results {
					results[i].LibraryID = lib.ID
				}
				if err := repo.Upsert(ctx, lib.ID, results, scan.UpsertOptions{}); err != nil {
					t.Fatalf("Upsert: %v", err)
				}
				moved = enq.RepointSettled(ctx, results)
				enqueued, _, err = enq.EnqueuePending(ctx, lib)
				if err != nil {
					t.Fatalf("EnqueuePending: %v", err)
				}
				return moved, enqueued
			}

			if err := testutil.WriteAudioFile(dir, "01 song.mp3", "Some Artist", "Some Title", "Album", ""); err != nil {
				t.Fatalf("write mp3: %v", err)
			}
			if _, n := rescan(); n != 1 {
				t.Fatalf("first scan enqueued %d, want 1", n)
			}
			item, err := q.Dequeue(ctx)
			if err != nil {
				t.Fatalf("Dequeue: %v", err)
			}
			if tc.sidecar {
				if err := os.WriteFile(filepath.Join(dir, "01 song.lrc"), []byte("[00:01.00]la\n"), 0o600); err != nil {
					t.Fatalf("write sidecar: %v", err)
				}
				err = q.Complete(ctx, item.ID)
			} else {
				_, err = q.RetireMiss(ctx, item.ID)
			}
			if err != nil {
				t.Fatalf("settle row: %v", err)
			}

			// Re-rip in place: the .mp3 is replaced by a same-stem .flac.
			if err := os.Remove(filepath.Join(dir, "01 song.mp3")); err != nil {
				t.Fatalf("remove mp3: %v", err)
			}
			if err := testutil.WriteFLACFileWithComments(dir, "01 song.flac", 44100, 44100*30,
				map[string]string{"ARTIST": "Some Artist", "TITLE": "Some Title"}); err != nil {
				t.Fatalf("write flac: %v", err)
			}
			if _, n := rescan(); n != tc.wantEnq {
				t.Fatalf("rescan enqueued %d, want %d", n, tc.wantEnq)
			}

			var rows int
			var source, status string
			if err := sqlDB.QueryRow(`SELECT COUNT(*), MAX(source_path), MAX(status) FROM work_queue`).
				Scan(&rows, &source, &status); err != nil {
				t.Fatalf("read work_queue: %v", err)
			}
			if rows != 1 || filepath.Base(source) != "01 song.flac" {
				t.Errorf("after the swap: %d row(s), source %q; want the one row naming 01 song.flac", rows, filepath.Base(source))
			}
			if status != tc.wantStatus {
				t.Errorf("moved row status = %s, want %s kept", status, tc.wantStatus)
			}
			results, err := repo.ListByLibrary(ctx, lib.ID)
			if err != nil {
				t.Fatalf("ListByLibrary: %v", err)
			}
			if len(results) != 1 || filepath.Base(results[0].FilePath) != "01 song.flac" || results[0].Status != scan.StatusDone {
				t.Errorf("scan_results after the swap = %+v; want only the .flac, done", results)
			}
			// Idempotent: nothing is left to repair or fetch.
			if moved, n := rescan(); moved != 0 || n != 0 {
				t.Errorf("third scan = (moved %d, enqueued %d), want (0, 0)", moved, n)
			}
		})
	}
}

// cancellingRepointer cancels the scan's context on its first call.
type cancellingRepointer struct {
	fakeWorkQueue
	cancel func()
	calls  int
}

func (c *cancellingRepointer) RepointGoneSource(context.Context, models.Inputs) (bool, error) {
	c.calls++
	c.cancel()
	return true, nil
}

// A shutdown mid-scan must not run (and warn about) one doomed repoint per file.
func TestRepointSettledStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rp := &cancellingRepointer{cancel: cancel}
	found := []models.ScanResult{{FilePath: "/m/a.flac", Status: scan.StatusDone}, {FilePath: "/m/b.flac", Status: scan.StatusDone}}
	if moved := (&scan.Enqueuer{Queue: rp}).RepointSettled(ctx, found); moved != 1 || rp.calls != 1 {
		t.Errorf("moved %d over %d calls, want the loop to stop after the 1 call that canceled", moved, rp.calls)
	}
}
