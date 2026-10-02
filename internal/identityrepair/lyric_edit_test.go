package identityrepair

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/canticle/internal/normalize"
)

// markEdited stamps the #481 Stage 2 hand-edit mark on a work_queue row.
func markEdited(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	if _, err := db.Exec(`UPDATE work_queue SET lyric_offset_ms = 250, lyric_edited_at = '2026-09-01T00:00:00Z' WHERE id = ?`, id); err != nil {
		t.Fatalf("mark edited %d: %v", id, err)
	}
}

// #1226: a hand-edited done row's identity is corrected, but it is NOT reopened
// (a re-fetch would replace the edited .lrc), its settle state survives, and the
// result counts it.
func TestRun_RekeyCorrectsEditedRowWithoutReopening(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "done", sr)
	stampSettleState(t, db, wq)
	markEdited(t, db, wq)

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed != 1 || res.QueueUpdated != 1 || res.EditHeld != 1 {
		t.Fatalf("Result = %+v; want Changed=1 QueueUpdated=1 EditHeld=1", res)
	}
	wantKey := normalize.NormalizeKey("Alpha; Bravo")
	if a, k, status := queueIdentity(t, db, wq); a != "Alpha; Bravo" || k != wantKey || status != "done" {
		t.Fatalf("work_queue = (%q,%q,%q); want (Alpha; Bravo,%q,done): an edited row is corrected but never reopened", a, k, status, wantKey)
	}
	if s := readSettleState(t, db, wq); !s.completedAt.Valid || !s.timingOutcome.Valid {
		t.Errorf("settle state cleared on an edited row (%+v); want it kept, the row was not reopened", s)
	}
	var edited sql.NullString
	if err := db.QueryRow(`SELECT lyric_edited_at FROM work_queue WHERE id = ?`, wq).Scan(&edited); err != nil || !edited.Valid {
		t.Errorf("hand-edit mark lost (%v, %v); want it kept", edited, err)
	}
}

// #1226: a merge would delete or reopen a hand-edited row, so the whole change
// is skipped (scan_results included, so the two never drift) and counted.
func TestRun_MergeSkipsWhenARowIsEdited(t *testing.T) {
	for _, tc := range []struct {
		name       string
		editedGood bool
	}{{"dropped row edited", false}, {"survivor edited", true}} {
		t.Run(tc.name, func(t *testing.T) {
			db := openDB(t)
			lib := seedLibrary(t, db)
			srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
			srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
			wqBad := seedQueue(t, db, "AlphaBravo", "", "done", srBad)
			wqGood := seedQueue(t, db, "Alpha; Bravo", "", "done", srGood)
			if tc.editedGood {
				markEdited(t, db, wqGood)
			} else {
				markEdited(t, db, wqBad)
			}

			reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
			res, err := New(db, reader.read).Run(context.Background(), Options{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.EditSkips != 1 || res.Changed != 0 || res.QueueMerged != 0 {
				t.Fatalf("Result = %+v; want EditSkips=1 Changed=0 QueueMerged=0", res)
			}
			if n := queueCount(t, db); n != 2 {
				t.Fatalf("work_queue count = %d; want 2 (no merge)", n)
			}
			for _, id := range []int64{wqBad, wqGood} {
				if _, _, status := queueIdentity(t, db, id); status != "done" {
					t.Errorf("row %d status = %q; want done (not reopened)", id, status)
				}
			}
			if a, _, _ := scanIdentity(t, db, srBad); a != "AlphaBravo" {
				t.Errorf("scan_results corrected to %q on a skipped change; want it untouched", a)
			}
		})
	}
}

// #1226: the divergence pass shares reconcileQueue, so an edited row it
// re-keys is corrected without a reopen and counted.
func TestRepairDivergence_RekeyKeepsEditedRowSettled(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "Alpha; Bravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "done", sr)
	markEdited(t, db, wq)

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Rekeyed != 1 || res.EditHeld != 1 {
		t.Fatalf("Result = %+v; want Rekeyed=1 EditHeld=1", res)
	}
	if _, k, status := queueIdentity(t, db, wq); k != normalize.NormalizeKey("Alpha; Bravo") || status != "done" {
		t.Fatalf("work_queue = (%q,%q); want the corrected key and done", k, status)
	}
}

// #1226 (Copilot 4162129629): the disagreement branch would unlink the
// diverged members (resetting them to pending) and, with no member left,
// DELETE the row and its mark, after which a fresh unmarked row overwrites the
// edited .lrc. An edited row is left whole and the group is counted skipped.
func TestRepairDivergence_DisagreementLeavesEditedRowWhole(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "apply", true: "dry run"}[dryRun], func(t *testing.T) {
			db := openDB(t)
			lib := seedLibrary(t, db)
			srA := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
			srB := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
			if _, err := db.Exec(`UPDATE scan_results SET artist_key = 'alphabravo' WHERE id IN (?, ?)`, srA, srB); err != nil {
				t.Fatalf("force shared key: %v", err)
			}
			wq := seedQueue(t, db, "AlphaBravo", "", "done", srA)
			if _, err := db.Exec(`UPDATE work_queue SET artist_key = 'alphabravo' WHERE id = ?`, wq); err != nil {
				t.Fatalf("force queue key: %v", err)
			}
			if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
				t.Fatalf("link srB: %v", err)
			}
			setScanIdentity(t, db, srA, "Alpha; Bravo")
			setScanIdentity(t, db, srB, "Charlie; Delta")
			markEdited(t, db, wq)
			if _, err := db.Exec(`UPDATE scan_results SET status = 'done' WHERE id IN (?, ?)`, srA, srB); err != nil {
				t.Fatalf("settle scans: %v", err)
			}

			res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{DryRun: dryRun})
			if err != nil {
				t.Fatalf("RepairDivergence: %v", err)
			}
			if res.EditSkips != 1 || res.Unlinked != 0 || res.Deleted != 0 {
				t.Fatalf("Result = %+v; want EditSkips=1 Unlinked=0 Deleted=0", res)
			}
			var edited sql.NullString
			if err := db.QueryRow(`SELECT lyric_edited_at FROM work_queue WHERE id = ?`, wq).Scan(&edited); err != nil || !edited.Valid {
				t.Fatalf("edited row or its mark is gone (%v, %v); want both kept", edited, err)
			}
			for _, sr := range []int64{srA, srB} {
				if !junctionLinked(t, db, wq, sr) {
					t.Errorf("scan_result %d unlinked from an edited row; want the link kept", sr)
				}
				if status := scanStatus(t, db, sr); status != "done" {
					t.Errorf("scan_result %d status = %q; want done, untouched", sr, status)
				}
			}
		})
	}
}

// #1226 (Copilot 4162129651): a dry run classifies hand-edited rows exactly as
// apply does, so the preview's Changed/EditHeld/EditSkips are what --yes does.
// Fixture: one edited row re-keyed in place (held) and one edited survivor a
// merge would collapse (skipped), plus one ordinary correction.
func TestRun_DryRunCountsMatchApplyForEditedRows(t *testing.T) {
	seed := func(t *testing.T) *sql.DB {
		db := openDB(t)
		lib := seedLibrary(t, db)
		// Held: edited, corrected key is free.
		sr1 := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
		wq1 := seedQueue(t, db, "AlphaBravo", "", "done", sr1)
		markEdited(t, db, wq1)
		// Skipped: the corrected key is held by an edited survivor.
		sr2 := seedScan(t, db, lib, "/m/2.mp3", "CharlieDelta", "", "Song")
		sr3 := seedScan(t, db, lib, "/m/3.mp3", "Charlie; Delta", "", "Song")
		seedQueue(t, db, "CharlieDelta", "", "done", sr2)
		wq3 := seedQueue(t, db, "Charlie; Delta", "", "done", sr3)
		markEdited(t, db, wq3)
		// Ordinary.
		sr4 := seedScan(t, db, lib, "/m/4.mp3", "EchoFoxtrot", "", "Song")
		seedQueue(t, db, "EchoFoxtrot", "", "done", sr4)
		return db
	}
	reader := fakeReader{
		"/m/1.mp3": {"Alpha; Bravo", ""},
		"/m/2.mp3": {"Charlie; Delta", ""},
		"/m/3.mp3": {"Charlie; Delta", ""},
		"/m/4.mp3": {"Echo; Foxtrot", ""},
	}
	dryDB := seed(t)
	dry, err := New(dryDB, reader.read).Run(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatalf("dry Run: %v", err)
	}
	if a, _, _ := scanIdentity(t, dryDB, 1); a != "AlphaBravo" {
		t.Errorf("dry run mutated scan_results: artist = %q", a)
	}
	applied, err := New(seed(t), reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("apply Run: %v", err)
	}
	if dry.Changed != 2 || dry.EditHeld != 1 || dry.EditSkips != 1 {
		t.Errorf("dry run = %+v; want Changed=2 EditHeld=1 EditSkips=1", dry)
	}
	if dry.Changed != applied.Changed || dry.EditHeld != applied.EditHeld || dry.EditSkips != applied.EditSkips {
		t.Errorf("dry run (Changed=%d EditHeld=%d EditSkips=%d) != apply (Changed=%d EditHeld=%d EditSkips=%d)",
			dry.Changed, dry.EditHeld, dry.EditSkips, applied.Changed, applied.EditHeld, applied.EditSkips)
	}
}
