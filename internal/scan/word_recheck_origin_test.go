package scan_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
)

// seedFlippedRecheck returns a library with one pending scan_result and a
// settled work_queue row for the same track flipped into word-recheck mode.
func seedFlippedRecheck(t *testing.T) (*sql.DB, models.Library, *scan.Repo, int64) {
	t.Helper()
	ctx := context.Background()
	dbh := openTestDB(t)
	lib, err := library.New(dbh).Add(ctx, "/m", "M", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("add library: %v", err)
	}
	repo := scan.New(dbh)
	if err := repo.Upsert(ctx, lib.ID, []models.ScanResult{{FilePath: "/m/x.flac",
		Track: models.Track{ArtistName: "A", TrackName: "t"}, Outdir: "/m", Filename: "x.lrc"}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var id int64
	if err := dbh.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, output_paths, status,
             outcome_type, timing_outcome, completed_at)
         VALUES ('A', 't', 'a', 't', '/m/x.flac', '[{"outdir":"/m","filename":"x.lrc"}]', 'done', 'synced', 'ok',
             '2026-01-10T00:00:00Z') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed work_queue: %v", err)
	}
	if prior, err := queue.NewDBQueue(dbh).MarkWordRecheckQueued(ctx, []int64{id}, queue.WordRecheckOptions{}, nil); err != nil || len(prior) != 1 {
		t.Fatalf("flip = (%d, %v); want 1", len(prior), err)
	}
	return dbh, lib, repo, id
}

func recheckRow(t *testing.T, dbh *sql.DB, id int64) (row string) {
	t.Helper()
	if err := dbh.QueryRow(`SELECT status || '|' || COALESCE(word_timing_state, '') || '|' || output_paths || '|' ||
        COALESCE(completed_at, '') || '|' || COALESCE(outcome_type, '') FROM work_queue WHERE id = ?`, id).Scan(&row); err != nil {
		t.Fatalf("read: %v", err)
	}
	return row
}

// TestInventoryWebhookCollisionKeepsWordRecheckRow (#1039, Copilot 4085542465):
// the Lidarr webhook's inventory path builds its inputs with ResultInputs, so
// they carry a ScanResultID, yet it is not a scan. It must leave a flipped row
// in recheck mode with its paths, byte-for-byte, while still linking the
// scan_result.
func TestInventoryWebhookCollisionKeepsWordRecheckRow(t *testing.T) {
	ctx := context.Background()
	dbh, _, repo, id := seedFlippedRecheck(t)
	results, err := repo.List(ctx, scan.Filter{})
	if err != nil || len(results) != 1 {
		t.Fatalf("list = (%d, %v); want 1", len(results), err)
	}
	in, err := scan.ResultInputs(results[0])
	if err != nil || in.ScanResultID == 0 || in.FromScan {
		t.Fatalf("ResultInputs = (ScanResultID %d, FromScan %v, %v); want a linked, unmarked input", in.ScanResultID, in.FromScan, err)
	}
	before := recheckRow(t, dbh, id)
	if _, err := queue.NewDBQueue(dbh).Enqueue(ctx, in, queue.PriorityWebhook); err != nil {
		t.Fatalf("webhook enqueue: %v", err)
	}
	if after := recheckRow(t, dbh, id); after != before {
		t.Fatalf("inventory webhook collision moved the row %q -> %q", before, after)
	}
	var links int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = ? AND scan_result_id = ?`,
		id, in.ScanResultID).Scan(&links); err != nil || links != 1 {
		t.Fatalf("junction links = (%d, %v); want 1 -- the webhook still links its scan_result", links, err)
	}
}

// TestScanEnqueueReopensWordRecheckRow drives the real scan enqueuer: it is
// the one caller that marks scan origin, so its collision reopens the row for
// an ordinary fetch.
func TestScanEnqueueReopensWordRecheckRow(t *testing.T) {
	ctx := context.Background()
	dbh, lib, repo, id := seedFlippedRecheck(t)
	enq := scan.Enqueuer{Results: repo, Cache: cache.New(dbh), Queue: queue.NewDBQueue(dbh), Priority: queue.PriorityScan}
	if n, _, err := enq.EnqueuePending(ctx, lib); err != nil || n != 1 {
		t.Fatalf("EnqueuePending = (%d, %v); want 1", n, err)
	}
	if got := recheckRow(t, dbh, id); got != `pending||[{"outdir":"/m","filename":"x.lrc"}]||` {
		t.Fatalf("scan collision left the row %q; want it reopened as an ordinary pending fetch", got)
	}
}
