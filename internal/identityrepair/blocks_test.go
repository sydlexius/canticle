package identityrepair

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/normalize"
)

// blockCount is how many lyric blocks sit under the artist's key for "Song".
func blockCount(t *testing.T, s *lyricblock.Store, artist string) int {
	t.Helper()
	got, err := s.List(context.Background(), lyricblock.ListFilter{
		ArtistKey: normalize.NormalizeKey(artist), TitleKey: normalize.NormalizeKey("Song")})
	if err != nil {
		t.Fatalf("list blocks: %v", err)
	}
	return len(got)
}

func addBlock(t *testing.T, s *lyricblock.Store, db *sql.DB, artist, fp string) {
	t.Helper()
	if _, err := s.Add(context.Background(), db, lyricblock.Block{ArtistKey: artist, TitleKey: "Song", Fingerprint: fp}); err != nil {
		t.Fatalf("add block: %v", err)
	}
}

// A re-keyed row carries its blocks to the corrected identity; none stay behind.
func TestRun_RekeyMovesBlocks(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "done", sr)
	store := lyricblock.NewStore(db, slog.Default())
	addBlock(t, store, db, "AlphaBravo", "fp1")
	addBlock(t, store, db, "AlphaBravo", "fp2")

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	if _, err := New(db, reader.read).WithBlocks(store).Run(context.Background(), Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := blockCount(t, store, "Alpha; Bravo"); n != 2 {
		t.Errorf("blocks under the new identity = %d, want 2", n)
	}
	if n := blockCount(t, store, "AlphaBravo"); n != 0 {
		t.Errorf("blocks left under the old identity = %d, want 0", n)
	}
}

// A merge moves the dropped row's blocks to the survivor. A fingerprint the
// survivor already holds is the duplicate and does not fail the repair.
func TestRun_MergeMovesBlocksAndToleratesDuplicates(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "pending", srBad)
	seedQueue(t, db, "Alpha; Bravo", "", "pending", srGood)
	store := lyricblock.NewStore(db, slog.Default())
	addBlock(t, store, db, "AlphaBravo", "shared")
	addBlock(t, store, db, "AlphaBravo", "only-old")
	addBlock(t, store, db, "Alpha; Bravo", "shared")

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
	if _, err := New(db, reader.read).WithBlocks(store).Run(context.Background(), Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := blockCount(t, store, "Alpha; Bravo"); n != 2 {
		t.Errorf("blocks under the survivor = %d, want 2 (shared once, plus only-old)", n)
	}
	if n := blockCount(t, store, "AlphaBravo"); n != 0 {
		t.Errorf("blocks left under the dropped identity = %d, want 0", n)
	}
}

// A dry run moves nothing, and a repair that rolls back leaves the blocks where
// they were: the move shares the row update's transaction.
func TestRun_BlocksStayOnDryRunAndRollback(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "done", sr)
	store := lyricblock.NewStore(db, slog.Default())
	addBlock(t, store, db, "AlphaBravo", "fp1")
	repairer := New(db, fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}.read).WithBlocks(store)

	if _, err := repairer.Run(context.Background(), Options{DryRun: true}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if blockCount(t, store, "AlphaBravo") != 1 || blockCount(t, store, "Alpha; Bravo") != 0 {
		t.Error("a dry run moved blocks")
	}

	_, err := repairer.Run(context.Background(), Options{Report: func(Change) error { return errReport }})
	if !errors.Is(err, errReport) {
		t.Fatalf("Run err = %v; want wrapped %v", err, errReport)
	}
	if blockCount(t, store, "AlphaBravo") != 1 || blockCount(t, store, "Alpha; Bravo") != 0 {
		t.Error("blocks moved although the repair rolled back")
	}
}

// noopBlocks is the explicit "this test does not care about blocks" mover.
type noopBlocks struct{}

func (noopBlocks) Rekey(context.Context, lyricblock.Execer, string, string, string, string) (int, error) {
	return 0, nil
}

// newRepairer is New with a no-op mover: a re-key with no mover is an error.
func newRepairer(db *sql.DB, read IdentityReader) *Repairer {
	return New(db, read).WithBlocks(noopBlocks{})
}

// Without a mover, a repair that re-keys a row fails instead of stranding blocks.
func TestRun_RekeyWithoutBlockMoverFails(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "done", sr)
	if _, err := New(db, fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}.read).Run(context.Background(), Options{}); err == nil {
		t.Fatal("Run succeeded with no block mover; want an error")
	}
}

func divStore(t *testing.T, db *sql.DB, artist, fp string) *lyricblock.Store {
	t.Helper()
	s := lyricblock.NewStore(db, slog.Default())
	addBlock(t, s, db, normalize.NormalizeKey(artist), fp)
	return s
}

// The divergence pass moves a re-keyed row's blocks to the corrected identity.
func TestRepairDivergence_RekeyMovesBlocks(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "done", sr)
	setScanIdentity(t, db, sr, "Alpha; Bravo")
	store := divStore(t, db, "AlphaBravo", "fp1")

	res, err := New(db, fakeReader{}.read).WithBlocks(store).RepairDivergence(context.Background(), Options{})
	if err != nil || res.Rekeyed != 1 {
		t.Fatalf("RepairDivergence = %+v, %v; want Rekeyed=1", res, err)
	}
	if blockCount(t, store, "Alpha; Bravo") != 1 || blockCount(t, store, "AlphaBravo") != 0 {
		t.Error("block did not move to the corrected identity")
	}
}

// The divergence pass moves the dropped row's blocks to the surviving row.
func TestRepairDivergence_MergeMovesBlocks(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "pending", srBad)
	seedQueue(t, db, "Alpha; Bravo", "", "done", srGood)
	setScanIdentity(t, db, srBad, "Alpha; Bravo")
	store := divStore(t, db, "AlphaBravo", "fp1")

	res, err := New(db, fakeReader{}.read).WithBlocks(store).RepairDivergence(context.Background(), Options{})
	if err != nil || res.Merged != 1 {
		t.Fatalf("RepairDivergence = %+v, %v; want Merged=1", res, err)
	}
	if blockCount(t, store, "Alpha; Bravo") != 1 || blockCount(t, store, "AlphaBravo") != 0 {
		t.Error("block did not move to the surviving identity")
	}
}

// A shared row whose members split to two different keys is deleted, and its
// blocks stay under the old key (no single destination): orphans, not lost.
func TestRepairDivergence_DisagreementLeavesBlocksAsOrphans(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srA := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srB := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", srA)
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
		t.Fatalf("link srB: %v", err)
	}
	setScanIdentity(t, db, srA, "Alpha; Bravo")
	setScanIdentity(t, db, srB, "Charlie; Delta")
	store := divStore(t, db, "AlphaBravo", "fp1")

	res, err := New(db, fakeReader{}.read).WithBlocks(store).RepairDivergence(context.Background(), Options{})
	if err != nil || res.Deleted != 1 {
		t.Fatalf("RepairDivergence = %+v, %v; want Deleted=1", res, err)
	}
	if blockCount(t, store, "AlphaBravo") != 1 {
		t.Error("blocks of the deleted row were moved or lost; want them left as orphans")
	}
}

// captureLogs routes slog.Default to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A shared row whose members correct to DIFFERENT keys has no single
// destination: Run re-keys the row but leaves the old identity's blocks put,
// and logs that once the commit landed.
func TestRun_DivergentSharedRowKeepsBlocksAtOldIdentity(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srA := seedScan(t, db, lib, "/m/1.mp3", "AB C", "", "Song")
	srB := seedScan(t, db, lib, "/m/2.mp3", "AB C", "", "Song")
	if _, err := db.Exec(`UPDATE scan_results SET artist_key = 'abc' WHERE id IN (?, ?)`, srA, srB); err != nil {
		t.Fatalf("force shared key: %v", err)
	}
	wq := seedQueue(t, db, "AB C", "", "pending", srA)
	if _, err := db.Exec(`UPDATE work_queue SET artist_key = 'abc' WHERE id = ?`, wq); err != nil {
		t.Fatalf("force queue key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
		t.Fatalf("link srB: %v", err)
	}
	store := lyricblock.NewStore(db, slog.Default())
	addBlock(t, store, db, "abc", "fp1")
	logs := captureLogs(t)

	reader := fakeReader{"/m/1.mp3": {"A; BC", ""}, "/m/2.mp3": {"AB; C", ""}}
	if _, err := New(db, reader.read).WithBlocks(store).Run(context.Background(), Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if blockCount(t, store, "A; BC") != 0 || blockCount(t, store, "AB; C") != 0 {
		t.Error("blocks landed on a member's new key although the members split")
	}
	if blockCount(t, store, "abc") != 1 {
		t.Error("blocks left the old identity")
	}
	if !strings.Contains(logs.String(), "left under the old key") {
		t.Errorf("no left-in-place log after commit: %s", logs.String())
	}
}

// A dry run deletes nothing, so it must not say it did.
func TestRepairDivergence_DryRunLogsAPlanNotADeletion(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srA := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srB := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", srA)
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
		t.Fatalf("link srB: %v", err)
	}
	setScanIdentity(t, db, srA, "Alpha; Bravo")
	setScanIdentity(t, db, srB, "Charlie; Delta")
	logs := captureLogs(t)
	if _, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{DryRun: true}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, "would be left") || strings.Contains(out, "blocks left under") {
		t.Errorf("dry-run log = %q; want a planned-action message only", out)
	}
}
