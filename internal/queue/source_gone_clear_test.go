package queue

import (
	"context"
	"database/sql"
	"testing"
)

const staleMark = "2026-07-31T12:00:00Z"

func markOf(t *testing.T, dbh *sql.DB, id int64) (m sql.NullString) {
	t.Helper()
	if err := dbh.QueryRow(`SELECT source_gone_at FROM work_queue WHERE id = ?`, id).Scan(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// #1597: a source_gone_at mark from an earlier life must not survive a reopen
// or a completed fetch, or it would (a) shortcut a later two-sighting retirement
// and (b) make prune's resurrect discriminator ("prune-retired implies NULL")
// false for a never-fetched row.
func TestSourceGoneMarkClearedByReopenAndComplete(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	id := seedUpgradeRow(t, dbh, "s", "source_gone_at = '"+staleMark+"'")

	tx, err := dbh.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ReopenDoneRowTx(ctx, tx, id, upgradeNow); err != nil || !ok {
		t.Fatalf("reopen = %v, %v", ok, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if m := markOf(t, dbh, id); m.Valid {
		t.Errorf("mark after ReopenDoneRowTx = %v, want NULL", m)
	}

	id2 := seedUpgradeRow(t, dbh, "c", "status = 'processing', source_gone_at = '"+staleMark+"'")
	if err := q.Complete(ctx, id2); err != nil {
		t.Fatal(err)
	}
	if m := markOf(t, dbh, id2); m.Valid {
		t.Errorf("mark after Complete = %v, want NULL", m)
	}
}
