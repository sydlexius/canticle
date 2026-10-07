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
