package queue

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

type goneRow struct {
	status, lastErr string
	armed, misses   int
	mark            sql.NullString
	outcome         string
}

func readGone(t *testing.T, dbh *sql.DB, id int64) (r goneRow) {
	t.Helper()
	if err := dbh.QueryRow(`SELECT status, last_error, upgrade_queued, upgrade_miss_count, source_gone_at, COALESCE(outcome_type, '')
        FROM work_queue WHERE id = ?`, id).Scan(&r.status, &r.lastErr, &r.armed, &r.misses, &r.mark, &r.outcome); err != nil {
		t.Fatal(err)
	}
	return r
}

const goneTrip = "status = 'processing', upgrade_queued = 1, attempts = 2"

// #1597: the first gone sighting settles the trip as an ANSWERED miss (the hold
// escalates) and marks the row; it does not retire.
func TestSettleGoneUpgradeTrip_FirstSightingMarksAndEscalates(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	id := seedUpgradeRow(t, dbh, "g", goneTrip)
	out, err := q.SettleGoneUpgradeTrip(ctx, id, 24*time.Hour)
	if err != nil || out != GoneMarked {
		t.Fatalf("SettleGoneUpgradeTrip = %v, %v; want GoneMarked", out, err)
	}
	r := readGone(t, dbh, id)
	if r.status != StatusDone || r.lastErr != "" || r.armed != 0 || r.misses != 1 || r.outcome != "unsynced" ||
		r.mark.String != formatTime(upgradeNow) {
		t.Errorf("row = %+v; want done, last_error empty, disarmed, 1 miss, outcome kept, mark=now", r)
	}
	if got, _ := q.ListUpgradeCandidates(ctx, upgradeNow.Add(365*24*time.Hour), 100); !slices.Contains(got, id) {
		t.Errorf("a marked (not retired) row must stay an upgrade candidate, got %v", got)
	}
}

// A later sighting with the mark at least 24h old retires; a younger mark does
// not, and is not reset.
func TestSettleGoneUpgradeTrip_SecondSighting(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		want GoneOutcome
	}{
		{"mark 25h old retires", 25 * time.Hour, GoneRetired},
		{"mark exactly 24h old retires", 24 * time.Hour, GoneRetired},
		{"mark 23h old does not", 23 * time.Hour, GoneMarked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			q, dbh := upgradeQueue(t)
			id := seedUpgradeRow(t, dbh, "g", goneTrip)
			mark := formatTime(upgradeNow.Add(-tc.age))
			mustExec(t, dbh, `UPDATE work_queue SET source_gone_at = ? WHERE id = ?`, mark, id)
			// A scan_results row linked and 'processing' must be written back done.
			mustExec(t, dbh, `INSERT INTO libraries (id, path, name) VALUES (1, '/m', 'lib')`)
			mustExec(t, dbh, `INSERT INTO scan_results (id, library_id, file_path, status) VALUES (1, 1, '/m/x.flac', 'processing')`)
			mustExec(t, dbh, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, 1)`, id)
			out, err := q.SettleGoneUpgradeTrip(ctx, id, 24*time.Hour)
			if err != nil || out != tc.want {
				t.Fatalf("SettleGoneUpgradeTrip = %v, %v; want %v", out, err, tc.want)
			}
			r := readGone(t, dbh, id)
			var scanStatus string
			if err := dbh.QueryRow(`SELECT status FROM scan_results WHERE id = 1`).Scan(&scanStatus); err != nil {
				t.Fatal(err)
			}
			if tc.want == GoneRetired {
				if r.status != StatusDone || r.lastErr != UnresolvableGoneError || r.armed != 0 || r.mark.String != mark || scanStatus != "done" {
					t.Errorf("retired row = %+v scan=%s; want done, the sentinel, disarmed, mark kept, scan done", r, scanStatus)
				}
				if got, _ := q.ListUpgradeCandidates(ctx, upgradeNow.Add(365*24*time.Hour), 100); slices.Contains(got, id) {
					t.Error("a retired row must not be an upgrade candidate")
				}
				return
			}
			if r.lastErr != "" || r.mark.String != mark || r.misses != 1 {
				t.Errorf("young-mark row = %+v; want settled unretired, mark unchanged, 1 miss", r)
			}
		})
	}
}

// Rows that are not a processing upgrade trip are untouched (a lost race).
func TestSettleGoneUpgradeTrip_NotATripChangesNothing(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	plain := seedUpgradeRow(t, dbh, "plain", "status = 'processing'")
	queued := seedUpgradeRow(t, dbh, "queued", "status = 'pending', upgrade_queued = 1")
	for _, id := range []int64{plain, queued} {
		mustExec(t, dbh, `UPDATE work_queue SET source_gone_at = ? WHERE id = ?`, formatTime(upgradeNow.Add(-48*time.Hour)), id)
		before := readGone(t, dbh, id)
		out, err := q.SettleGoneUpgradeTrip(ctx, id, 24*time.Hour)
		if err != nil || out != GoneNotTrip {
			t.Fatalf("SettleGoneUpgradeTrip(%d) = %v, %v; want GoneNotTrip", id, out, err)
		}
		if after := readGone(t, dbh, id); after != before {
			t.Errorf("row %d changed: %+v -> %+v", id, before, after)
		}
	}
}

func TestClearSourceGone(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	id := seedUpgradeRow(t, dbh, "c", "source_gone_at = '2026-09-01T00:00:00Z'")
	if err := q.ClearSourceGone(ctx, id); err != nil {
		t.Fatal(err)
	}
	if r := readGone(t, dbh, id); r.mark.Valid {
		t.Errorf("mark = %q; want NULL", r.mark.String)
	}
	if err := q.ClearSourceGone(ctx, id); err != nil {
		t.Errorf("clearing an unmarked row: %v", err)
	}
}

// A mark from an earlier life must not turn a later single sighting into a
// retirement: the reopen and the completed fetch both void it, so the next gone
// sighting is a first one again (the two-sighting rule, #1597).
func TestSettleGoneUpgradeTrip_StaleMarkDoesNotRetireOnOneSighting(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	id := seedUpgradeRow(t, dbh, "s", goneTrip)
	q.now = func() time.Time { return upgradeNow.Add(-60 * 24 * time.Hour) }
	if out, err := q.SettleGoneUpgradeTrip(ctx, id, 24*time.Hour); err != nil || out != GoneMarked {
		t.Fatalf("first = %v %v", out, err)
	}
	q.now = func() time.Time { return upgradeNow }
	tx, err := dbh.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ReopenDoneRowTx(ctx, tx, id, upgradeNow.Add(-59*24*time.Hour)); err != nil || !ok {
		t.Fatalf("reopen %v %v", ok, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, dbh, `UPDATE work_queue SET status='processing' WHERE id = ?`, id)
	if err := q.Complete(ctx, id); err != nil {
		t.Fatal(err)
	}
	mustExec(t, dbh, `UPDATE work_queue SET outcome_type='unsynced' WHERE id = ?`, id)
	mustExec(t, dbh, `UPDATE work_queue SET `+goneTrip+` WHERE id = ?`, id)
	out, err := q.SettleGoneUpgradeTrip(ctx, id, 24*time.Hour)
	if err != nil || out != GoneMarked {
		t.Fatalf("single post-reopen sighting = %v, %v; want GoneMarked (the old mark must be gone)", out, err)
	}
}
