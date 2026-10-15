package identityrepair

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"testing"

	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/normalize"
)

// seedEdition is seedQueue for a row of the given edition (#1561).
func seedEdition(t *testing.T, db *sql.DB, artist, status, edition string, scanID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, edition, status, output_paths)
         VALUES (?, 'Song', ?, 'song', ?, ?, ?) RETURNING id`,
		artist, normalize.NormalizeKey(artist), edition, status, fmt.Sprintf(`[{"outdir":"/out","filename":"f%d.lrc"}]`, scanID)).Scan(&id); err != nil {
		t.Fatalf("seed %q work_queue: %v", edition, err)
	}
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, scanID); err != nil {
		t.Fatalf("seed junction: %v", err)
	}
	return id
}

type queueSnap struct {
	artistKey, status, edition, outputs string
	links                               int
}

func snapQueue(t *testing.T, db *sql.DB, id int64) queueSnap {
	t.Helper()
	var s queueSnap
	if err := db.QueryRow(`SELECT artist_key, status, edition, output_paths,
            (SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = work_queue.id)
         FROM work_queue WHERE id = ?`, id).Scan(&s.artistKey, &s.status, &s.edition, &s.outputs, &s.links); err != nil {
		t.Fatalf("read work_queue %d: %v", id, err)
	}
	return s
}

// Run re-keys the edition row its corrected scan row is linked to and leaves
// the twin on the other edition, and the shared blocks, where they are.
func TestRun_RekeysTheLinkedEditionRowOnly(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srSung := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srInst := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
	sung := seedEdition(t, db, "AlphaBravo", "done", "", srSung)
	inst := seedEdition(t, db, "AlphaBravo", "done", "instrumental", srInst)
	store := lyricblock.NewStore(db, slog.Default())
	addBlock(t, store, db, "AlphaBravo", "fp1")
	before := snapQueue(t, db, sung)

	reader := fakeReader{"/m/1.mp3": {"AlphaBravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).WithBlocks(store).Run(context.Background(), Options{})
	if err != nil || res.QueueUpdated != 1 || res.QueueMerged != 0 {
		t.Fatalf("Run = %+v, %v; want one re-key, no merge", res, err)
	}
	if got := snapQueue(t, db, inst); got.artistKey != "alpha; bravo" || got.edition != "instrumental" {
		t.Errorf("instrumental row = %+v; want re-keyed to alpha; bravo, edition kept", got)
	}
	if got := snapQueue(t, db, sung); got != before {
		t.Errorf("sung twin changed: %+v; want %+v", got, before)
	}
	if n := blockCount(t, store, "AlphaBravo"); n != 1 {
		t.Errorf("blocks under the twin's keys = %d; want 1 (they stay with the twin)", n)
	}
}

// When the empty-edition (sung) row is the one re-keyed, its blocks follow it even though
// the instrumental twin keeps the old keys: the sung row must never be
// re-fetched at its corrected identity without them (#1397).
func TestRun_RekeyedSungRowTakesItsBlocks(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srSung := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srInst := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
	sung := seedEdition(t, db, "AlphaBravo", "done", "", srSung)
	inst := seedEdition(t, db, "AlphaBravo", "done", "instrumental", srInst)
	store := lyricblock.NewStore(db, slog.Default())
	addBlock(t, store, db, "AlphaBravo", "fp1")
	before := snapQueue(t, db, inst)

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}, "/m/2.mp3": {"AlphaBravo", ""}}
	res, err := New(db, reader.read).WithBlocks(store).Run(context.Background(), Options{})
	if err != nil || res.QueueUpdated != 1 || res.QueueMerged != 0 {
		t.Fatalf("Run = %+v, %v; want one re-key, no merge", res, err)
	}
	if got := snapQueue(t, db, sung); got.artistKey != "alpha; bravo" || got.edition != "" {
		t.Errorf("sung row = %+v; want re-keyed to alpha; bravo, edition ''", got)
	}
	if got := snapQueue(t, db, inst); got != before {
		t.Errorf("instrumental twin changed: %+v; want %+v", got, before)
	}
	if n := blockCount(t, store, "Alpha; Bravo"); n != 1 {
		t.Errorf("blocks under the sung row's new keys = %d; want 1 (they follow the '' row)", n)
	}
	if n := blockCount(t, store, "AlphaBravo"); n != 0 {
		t.Errorf("blocks left under the old keys = %d; want 0", n)
	}
}

// A corrected key held by the other edition's row is no conflict: the edition
// row is re-keyed beside it, never merged into it.
func TestRun_NeverMergesAcrossEditions(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srGood := seedScan(t, db, lib, "/m/1.mp3", "Alpha; Bravo", "", "Song")
	srBad := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
	sung := seedEdition(t, db, "Alpha; Bravo", "done", "", srGood)
	inst := seedEdition(t, db, "AlphaBravo", "pending", "instrumental", srBad)
	before := snapQueue(t, db, sung)

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).WithBlocks(lyricblock.NewStore(db, slog.Default())).Run(context.Background(), Options{})
	if err != nil || res.QueueMerged != 0 || res.QueueUpdated != 1 {
		t.Fatalf("Run = %+v, %v; want a re-key and no merge", res, err)
	}
	if n := queueCount(t, db); n != 2 {
		t.Fatalf("rows = %d; want both editions kept", n)
	}
	if got := snapQueue(t, db, inst); got.artistKey != "alpha; bravo" || got.edition != "instrumental" {
		t.Errorf("instrumental row = %+v; want re-keyed beside its twin", got)
	}
	if got := snapQueue(t, db, sung); got != before {
		t.Errorf("sung row changed: %+v; want %+v", got, before)
	}
}

// A scan row linked to no row at its old keys never picks an edition row: that
// row belongs to its own linked members.
func TestRun_UnlinkedScanRowLeavesTheEditionRowAlone(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srInst := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	inst := seedEdition(t, db, "AlphaBravo", "done", "instrumental", srInst)
	seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song") // never enqueued
	before := snapQueue(t, db, inst)

	reader := fakeReader{"/m/1.mp3": {"AlphaBravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).WithBlocks(lyricblock.NewStore(db, slog.Default())).Run(context.Background(), Options{})
	if err != nil || res.Changed != 1 || res.QueueUpdated != 0 {
		t.Fatalf("Run = %+v, %v; want the scan row corrected and no queue change", res, err)
	}
	if got := snapQueue(t, db, inst); got != before {
		t.Errorf("instrumental row changed: %+v; want %+v", got, before)
	}
}

// RepairDivergence re-keys the divergent row itself, not the other edition's
// row that shares its old keys.
func TestRepairDivergence_RekeysTheDivergentEditionRow(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srSung := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srInst := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song") // already corrected
	sung := seedEdition(t, db, "AlphaBravo", "done", "", srSung)
	inst := seedEdition(t, db, "AlphaBravo", "done", "instrumental", srInst)
	before := snapQueue(t, db, sung)

	res, err := New(db, fakeReader{}.read).WithBlocks(lyricblock.NewStore(db, slog.Default())).RepairDivergence(context.Background(), Options{})
	if err != nil || res.Rekeyed != 1 || res.Merged != 0 {
		t.Fatalf("RepairDivergence = %+v, %v; want one re-key", res, err)
	}
	if got := snapQueue(t, db, inst); got.artistKey != "alpha; bravo" {
		t.Errorf("instrumental row = %+v; want re-keyed", got)
	}
	if got := snapQueue(t, db, sung); got != before {
		t.Errorf("sung twin changed: %+v; want %+v", got, before)
	}
}

// The divergent row is named by id, not found again by its keys or its links:
// a member linked to both editions' rows still re-keys only the divergent one.
func TestRepairDivergence_NamesTheEditionRowEvenWhenAMemberLinksBoth(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srInst := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song") // already corrected
	srSung := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	inst := seedEdition(t, db, "AlphaBravo", "done", "instrumental", srInst)
	sung := seedEdition(t, db, "AlphaBravo", "done", "", srSung)
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, sung, srInst); err != nil {
		t.Fatal(err)
	}

	if _, err := New(db, fakeReader{}.read).WithBlocks(lyricblock.NewStore(db, slog.Default())).RepairDivergence(context.Background(), Options{}); err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if got := snapQueue(t, db, inst); got.artistKey != "alpha; bravo" {
		t.Errorf("instrumental row key = %q; want re-keyed", got.artistKey)
	}
	if got := snapQueue(t, db, sung); got.artistKey != "alphabravo" {
		t.Errorf("sung row key = %q; want it left at its own key", got.artistKey)
	}
}

// A merge within one edition still happens, and the shared blocks stay with
// the twin that keeps the old keys.
func TestRun_MergesWithinAnEditionAndLeavesTheTwinsBlocks(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srSung := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srBad := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/3.mp3", "Alpha; Bravo", "", "Song")
	sung := seedEdition(t, db, "AlphaBravo", "done", "", srSung)
	seedEdition(t, db, "AlphaBravo", "pending", "instrumental", srBad)
	keep := seedEdition(t, db, "Alpha; Bravo", "pending", "instrumental", srGood)
	store := lyricblock.NewStore(db, slog.Default())
	addBlock(t, store, db, "AlphaBravo", "fp1")
	before := snapQueue(t, db, sung)

	reader := fakeReader{"/m/1.mp3": {"AlphaBravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}, "/m/3.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).WithBlocks(store).Run(context.Background(), Options{})
	if err != nil || res.QueueMerged != 1 {
		t.Fatalf("Run = %+v, %v; want one merge", res, err)
	}
	if got := snapQueue(t, db, keep); got.links != 2 {
		t.Errorf("survivor links = %d; want 2", got.links)
	}
	if got := snapQueue(t, db, sung); got != before {
		t.Errorf("sung twin changed: %+v; want %+v", got, before)
	}
	if n := blockCount(t, store, "AlphaBravo"); n != 1 {
		t.Errorf("blocks under the twin's keys = %d; want 1", n)
	}
}
