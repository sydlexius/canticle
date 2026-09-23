package identityrepair

import (
	"context"
	"database/sql"
	"testing"
)

// wordState reads a work_queue row's status and word_timing_state.
func wordState(t *testing.T, db *sql.DB, id int64) (status, state string) {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRow(`SELECT status, word_timing_state FROM work_queue WHERE id = ?`, id).Scan(&status, &s); err != nil {
		t.Fatalf("read work_queue %d: %v", id, err)
	}
	return status, s.String
}

// A word-recheck row (#982: 'deferred' + 'queued') whose identity is corrected
// is reopened for an ordinary fetch under the new key (#1039); a plain
// 'deferred' row keeps its status, as before.
func TestRun_RekeyReopensWordRecheckRow(t *testing.T) {
	for _, tc := range []struct {
		state, wantStatus, wantState string
	}{
		{"queued", "pending", ""},
		{"absent", "deferred", "absent"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			db := openDB(t)
			lib := seedLibrary(t, db)
			sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
			wq := seedQueue(t, db, "AlphaBravo", "", "deferred", sr)
			if _, err := db.Exec(`UPDATE work_queue SET word_timing_state = ? WHERE id = ?`, tc.state, wq); err != nil {
				t.Fatalf("stamp state: %v", err)
			}
			reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
			if _, err := New(db, reader.read).Run(context.Background(), Options{}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if st, ws := wordState(t, db, wq); st != tc.wantStatus || ws != tc.wantState {
				t.Fatalf("work_queue = (%q,%q); want (%q,%q)", st, ws, tc.wantStatus, tc.wantState)
			}
		})
	}
}

// A merge into a word-recheck survivor reopens it, so the unioned path is
// fetched rather than skipped by a recheck that writes only word results.
func TestRun_MergeReopensWordRecheckSurvivor(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "pending", srBad)
	wqGood := seedQueue(t, db, "Alpha; Bravo", "", "deferred", srGood)
	if _, err := db.Exec(`UPDATE work_queue SET word_timing_state = 'queued' WHERE id = ?`, wqGood); err != nil {
		t.Fatalf("stamp state: %v", err)
	}
	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.QueueMerged != 1 {
		t.Fatalf("Result = %+v; want QueueMerged=1", res)
	}
	if st, ws := wordState(t, db, wqGood); st != "pending" || ws != "" {
		t.Fatalf("survivor = (%q,%q); want (pending, NULL)", st, ws)
	}
}

// The divergence pass's unanimous re-key shares reconcileQueue, so a recheck
// row reopens there too.
func TestRepairDivergence_RekeyReopensWordRecheckRow(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "deferred", sr)
	if _, err := db.Exec(`UPDATE work_queue SET word_timing_state = 'queued' WHERE id = ?`, wq); err != nil {
		t.Fatalf("stamp state: %v", err)
	}
	setScanIdentity(t, db, sr, "Alpha; Bravo")
	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil || res.Rekeyed != 1 {
		t.Fatalf("RepairDivergence = (%+v, %v); want Rekeyed=1", res, err)
	}
	if st, ws := wordState(t, db, wq); st != "pending" || ws != "" {
		t.Fatalf("work_queue = (%q,%q); want (pending, NULL)", st, ws)
	}
}
