package identityrepair

import (
	"context"
	"testing"
)

// #1405: a manually marked instrumental row is protected like a hand-edited
// one: a merge that would drop or reopen it is skipped, and an unmarked pair
// still merges.
func TestRun_MergeSkipsWhenARowIsManuallyMarked(t *testing.T) {
	for _, marked := range []bool{true, false} {
		db := openDB(t)
		lib := seedLibrary(t, db)
		srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
		srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
		wqBad := seedQueue(t, db, "AlphaBravo", "", "done", srBad)
		seedQueue(t, db, "Alpha; Bravo", "", "done", srGood)
		if marked {
			if _, err := db.Exec(`UPDATE work_queue SET manual_instrumental_at = '2026-09-01T00:00:00Z' WHERE id = ?`, wqBad); err != nil {
				t.Fatal(err)
			}
		}
		reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
		res, err := New(db, reader.read).Run(context.Background(), Options{})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		wantSkips, wantRows := 1, 2
		if !marked {
			wantSkips, wantRows = 0, 1
		}
		if res.EditSkips != wantSkips || queueCount(t, db) != wantRows {
			t.Errorf("marked=%v: EditSkips=%d rows=%d, want %d/%d (%+v)", marked, res.EditSkips, queueCount(t, db), wantSkips, wantRows, res)
		}
	}
}

// #1405: a disagreeing group on a manually marked row is skipped whole by the
// divergence pass (no unlink, no delete), where an unmarked row would be
// unlinked and deleted.
func TestRepairDivergence_DisagreementSkipsManuallyMarkedRow(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srA := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srB := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
	if _, err := db.Exec(`UPDATE scan_results SET artist_key = 'alphabravo' WHERE id IN (?, ?)`, srA, srB); err != nil {
		t.Fatal(err)
	}
	wq := seedQueue(t, db, "AlphaBravo", "", "done", srA)
	if _, err := db.Exec(`UPDATE work_queue SET artist_key = 'alphabravo', manual_instrumental_at = '2026-09-01T00:00:00Z' WHERE id = ?`, wq); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Alpha; Bravo', artist_key = 'alphabravo2' WHERE id = ?`, srA); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Charlie; Delta', artist_key = 'charliedelta' WHERE id = ?`, srB); err != nil {
		t.Fatal(err)
	}

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.EditSkips != 1 || res.Unlinked != 0 || res.Deleted != 0 {
		t.Fatalf("Result = %+v; want EditSkips=1 Unlinked=0 Deleted=0", res)
	}
	if queueCount(t, db) != 1 || !junctionLinked(t, db, wq, srA) || !junctionLinked(t, db, wq, srB) {
		t.Errorf("marked row or its links were touched")
	}
}
