package queue

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

var upgradeNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// seedUpgradeRow inserts a full upgrade candidate (a settled unsynced row,
// completed a month ago) and applies set, a SQL SET fragment, to vary one arm.
func seedUpgradeRow(t *testing.T, dbh *sql.DB, key, set string) int64 {
	t.Helper()
	var id int64
	if err := dbh.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type, completed_at)
         VALUES ('A', ?, 'a', ?, '/m/x.flac', 'done', 'unsynced', '2026-08-01T00:00:00Z') RETURNING id`, key, key).Scan(&id); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
	if set != "" {
		if _, err := dbh.Exec(`UPDATE work_queue SET `+set+` WHERE id = ?`, id); err != nil {
			t.Fatalf("vary %s: %v", key, err)
		}
	}
	return id
}

func upgradeQueue(t *testing.T) (*DBQueue, *sql.DB) {
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	q.now = func() time.Time { return upgradeNow }
	return q, dbh
}

func TestUpgradeCandidates_EachArm(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	hold := upgradeNow.Add(-7 * 24 * time.Hour)
	want := []int64{
		seedUpgradeRow(t, dbh, "unsynced", ""),
		seedUpgradeRow(t, dbh, "marker", "outcome_type = 'instrumental'"),
		seedUpgradeRow(t, dbh, "untimed-lrc", "outcome_type = 'synced', sync_tier = 'unsynced'"),
		seedUpgradeRow(t, dbh, "held-long-ago", "upgrade_checked_at = '2026-08-02T00:00:00Z'"),
	}
	for _, c := range []struct{ key, set string }{
		{"line", "outcome_type = 'synced', sync_tier = 'line'"},
		{"word", "outcome_type = 'synced', sync_tier = 'word'"},
		{"synced-untiered", "outcome_type = 'synced'"},
		{"rejected", "outcome_type = 'rejected'"},
		{"unknown", "outcome_type = NULL"},
		{"pending", "status = 'pending'"},
		{"unavailable", "status = 'unavailable'"},
		{"mis-synced", "timing_outcome = 'mis_synced'"},
		{"categorical", "timing_outcome = 'categorical'"},
		{"degenerate", "timing_outcome = 'degenerate'"},
		{"word-recheck", "word_timing_state = 'queued'"},
		{"no-source", "source_path = ' '"},
		{"retired-gone", "last_error = 'source file is gone'"},
		{"settled-this-week", "completed_at = '2026-09-25T00:00:00Z'"},
		{"admitted-this-week", "upgrade_checked_at = '2026-09-25T00:00:00Z'"},
	} {
		seedUpgradeRow(t, dbh, c.key, c.set)
	}
	got, err := q.ListUpgradeCandidates(ctx, hold, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	if got, _ := q.ListUpgradeCandidates(ctx, hold, 1); !slices.Equal(got, want[:1]) {
		t.Fatalf("limit 1 = %v, want never-admitted first %v", got, want[:1])
	}
}

// TestMarkUpgradeQueued_FlipStampAndHold: flip + arm + stamp, the trigger
// disarms on settle, and the stamp holds the row out after it settles.
func TestMarkUpgradeQueued_FlipStampAndHold(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	hold := upgradeNow.Add(-7 * 24 * time.Hour)
	id := seedUpgradeRow(t, dbh, "t", "attempts = 2, miss_count = 3, refused_waits = 1")
	line := seedUpgradeRow(t, dbh, "line", "outcome_type = 'synced', sync_tier = 'line'")
	flipped, err := q.MarkUpgradeQueued(ctx, []int64{id, line}, hold)
	if err != nil || !slices.Equal(flipped, []int64{id}) {
		t.Fatalf("flip = %v, %v; want [%d] (the stale line id revalidated out)", flipped, err, id)
	}
	var status, outcome string
	var checked sql.NullString
	var prio, attempts, misses, waits, armed int
	if err := dbh.QueryRow(`SELECT status, priority, attempts, miss_count, refused_waits, upgrade_checked_at, outcome_type, upgrade_queued
	      FROM work_queue WHERE id = ?`, id).
		Scan(&status, &prio, &attempts, &misses, &waits, &checked, &outcome, &armed); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != StatusPending || prio != PriorityMiss || attempts != 0 || waits != 0 || misses != 3 ||
		checked.String != formatTime(upgradeNow) || outcome != "unsynced" || armed != 1 {
		t.Fatalf("flipped row = %s prio=%d attempts=%d misses=%d waits=%d checked=%s outcome=%s armed=%d",
			status, prio, attempts, misses, waits, checked.String, outcome, armed)
	}
	if n, _ := q.CountUpgradeInFlight(ctx); n != 1 {
		t.Fatalf("in flight = %d, want 1", n)
	}
	// Any settle disarms it (the 053 trigger), here a raw write like prune's.
	if _, err := dbh.Exec(`UPDATE work_queue SET status = 'done' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.CountUpgradeInFlight(ctx); n != 0 {
		t.Fatalf("in flight after settle = %d, want 0", n)
	}
	if got, _ := q.ListUpgradeCandidates(ctx, hold, 100); len(got) != 0 {
		t.Fatalf("held row re-admitted: %v", got)
	}
	if got, _ := q.ListUpgradeCandidates(ctx, upgradeNow.Add(time.Second), 100); !slices.Equal(got, []int64{id}) {
		t.Fatalf("after the hold = %v, want [%d]", got, id)
	}
}

func TestSettleUpgradeTrip(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	up := seedUpgradeRow(t, dbh, "up", "status = 'processing', upgrade_queued = 1, attempts = 2, timing_outcome = 'ok', sync_tier = 'unsynced'")
	plain := seedUpgradeRow(t, dbh, "plain", "status = 'processing'")
	queued := seedUpgradeRow(t, dbh, "queued", "status = 'pending', upgrade_queued = 1")
	for _, c := range []struct {
		id   int64
		want bool
	}{{up, true}, {plain, false}, {queued, false}} {
		got, err := q.SettleUpgradeTrip(ctx, c.id)
		if err != nil || got != c.want {
			t.Fatalf("SettleUpgradeTrip(%d) = %v, %v; want %v", c.id, got, err, c.want)
		}
	}
	var status, completed, outcome, timing, tier string
	var attempts, armed int
	if err := dbh.QueryRow(`SELECT status, completed_at, outcome_type, timing_outcome, sync_tier, attempts, upgrade_queued FROM work_queue WHERE id = ?`, up).
		Scan(&status, &completed, &outcome, &timing, &tier, &attempts, &armed); err != nil {
		t.Fatal(err)
	}
	if status != StatusDone || completed != "2026-08-01T00:00:00Z" || outcome != "unsynced" || timing != "ok" || tier != "unsynced" || attempts != 0 || armed != 0 {
		t.Fatalf("settled row = %s completed_at=%s outcome=%s timing=%s tier=%s attempts=%d armed=%d; want done, file record unchanged, disarmed",
			status, completed, outcome, timing, tier, attempts, armed)
	}
}

// TestUpgradeTripSurvivesCollisionAndCancel (#553 review I-2): a scan or
// webhook Enqueue collision keeps the trip armed and its completion time, and
// neither Cleanup nor a library cancel deletes the settled row.
func TestUpgradeTripSurvivesCollisionAndCancel(t *testing.T) {
	ctx := context.Background()
	for _, fromScan := range []bool{false, true} {
		q, dbh := upgradeQueue(t)
		lib := addLibrary(t, dbh, "L", "/m")
		id := seedUpgradeRow(t, dbh, "t", "")
		linkScanResult(t, dbh, id, addScanResultIn(t, dbh, lib, "/m/x.flac", "/m", "x.lrc"))
		if _, err := q.MarkUpgradeQueued(ctx, []int64{id}, upgradeNow.Add(-7*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		in := models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "t"}, Outdir: "/m", Filename: "x.lrc", SourcePath: "/m/x.flac", FromScan: fromScan}
		var outdir0, filename0, source0 string
		if err := dbh.QueryRow(`SELECT outdir, filename, source_path FROM work_queue WHERE id = ?`, id).Scan(&outdir0, &filename0, &source0); err != nil {
			t.Fatal(err)
		}
		// R2-I1: a colliding enqueue from a DIFFERENT copy of the track must
		// not rewrite the armed row's identity, or the trip judges and writes
		// against a file it never selected.
		collide := in
		collide.Outdir, collide.Filename, collide.SourcePath = "/m/copy", "y.lrc", "/m/copy/x.flac"
		if _, err := q.Enqueue(ctx, collide, PriorityScan); err != nil {
			t.Fatal(err)
		}
		var outdir1, filename1, source1 string
		if err := dbh.QueryRow(`SELECT outdir, filename, source_path FROM work_queue WHERE id = ?`, id).Scan(&outdir1, &filename1, &source1); err != nil {
			t.Fatal(err)
		}
		if outdir1 != outdir0 || filename1 != filename0 || source1 != source0 {
			t.Fatalf("fromScan=%v: collision rewrote identity to (%q, %q, %q); want kept (%q, %q, %q)", fromScan, outdir1, filename1, source1, outdir0, filename0, source0)
		}
		var completed sql.NullString
		var rows int
		if err := dbh.QueryRow(`SELECT completed_at, (SELECT COUNT(*) FROM work_queue) FROM work_queue WHERE id = ?`, id).Scan(&completed, &rows); err != nil {
			t.Fatal(err)
		}
		if n, _ := q.CountUpgradeInFlight(ctx); n != 1 || rows != 1 || completed.String != "2026-08-01T00:00:00Z" {
			t.Fatalf("fromScan=%v: in flight=%d rows=%d completed_at=%v after collision; want 1, 1 (a collision), kept", fromScan, n, rows, completed)
		}
		if n, err := q.Cleanup(ctx, in); err != nil || n != 0 {
			t.Fatalf("fromScan=%v: Cleanup deleted %d, %v; want 0", fromScan, n, err)
		}
		if n, _, err := q.CancelByLibrary(ctx, lib); err != nil || n != 0 {
			t.Fatalf("fromScan=%v: CancelByLibrary deleted %d, %v; want 0", fromScan, n, err)
		}
	}
}
