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
		{"mis-synced-at-fetch", "timing_outcome = 'mis_synced', timing_stamp_source = 'fetch'"},
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
	if status != StatusPending || prio != PriorityUpgrade || attempts != 0 || waits != 0 || misses != 3 ||
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
		got, err := q.SettleUpgradeTrip(ctx, c.id, true)
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

// missyncedRow seeds a settled mis_synced row (a kept .lrc, so outcome_type is
// not an upgrade-arm one) and applies set to vary one term of the #1120 arm.
func missyncedRow(t *testing.T, dbh *sql.DB, key, set string) int64 {
	t.Helper()
	base := "outcome_type = 'synced', sync_tier = 'line', timing_outcome = 'mis_synced', timing_stamp_source = 'sweep'"
	if set != "" {
		base += ", " + set
	}
	return seedUpgradeRow(t, dbh, key, base)
}

// TestUpgradeCandidates_MissyncedArm (#1120): one row per inclusion and one per
// exclusion, each differing from a full candidate in exactly one term, under
// providers generation 9.
func TestUpgradeCandidates_MissyncedArm(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	q.SetProvidersVersion(9)
	want := []int64{
		missyncedRow(t, dbh, "sweep", ""),
		missyncedRow(t, dbh, "revalidate", "timing_stamp_source = 'revalidate'"),
		missyncedRow(t, dbh, "pre-055-unknown-source", "timing_stamp_source = NULL"),
		missyncedRow(t, dbh, "lane-set-changed", "missync_recheck_generation = 8"),
		// Recently settled and admitted, but judged since: a first pass under this verdict has no hold.
		missyncedRow(t, dbh, "fresh", "completed_at = '2026-09-28T00:00:00Z', upgrade_checked_at = '2026-09-28T00:00:00Z', evaluated_at = '2026-09-28T01:00:00Z'"),
		// A re-admission under the same verdict waits out the week hold.
		missyncedRow(t, dbh, "readmit-after-hold", "upgrade_checked_at = '2026-08-02T00:00:00Z', evaluated_at = '2026-08-01T00:00:00Z'"),
	}
	for _, c := range []struct{ key, set string }{
		{"fetch-guard", "timing_stamp_source = 'fetch'"},
		{"already-passed", "missync_recheck_generation = 9"},
		{"readmit-within-hold", "upgrade_checked_at = '2026-09-28T00:00:00Z', evaluated_at = '2026-09-27T00:00:00Z'"},
		{"ok", "timing_outcome = 'ok'"},
		{"categorical", "timing_outcome = 'categorical'"},
		{"pending", "status = 'pending'"},
		{"word-recheck", "word_timing_state = 'queued'"},
		{"no-source", "source_path = ' '"},
		{"retired-gone", "last_error = 'source file is gone'"},
	} {
		missyncedRow(t, dbh, c.key, c.set)
	}
	got, err := q.ListUpgradeCandidates(ctx, upgradeNow.Add(-7*24*time.Hour), 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

// TestMarkUpgradeQueued_MissyncedPassMarker (#1120): admission records no
// pass; a settle the lanes answered records it under the providers generation
// (not re-offered under it), one they did not answer leaves it unrecorded and
// re-offers the row after the week hold, and a lane-set change re-opens it.
func TestMarkUpgradeQueued_MissyncedPassMarker(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	q.SetProvidersVersion(9)
	hold := upgradeNow.Add(-7 * 24 * time.Hour)
	afterHold := upgradeNow.Add(8 * 24 * time.Hour)
	id := missyncedRow(t, dbh, "m", "")
	fetch := missyncedRow(t, dbh, "f", "timing_stamp_source = 'fetch'")
	marker := func() sql.NullInt64 {
		var g sql.NullInt64
		if err := dbh.QueryRow(`SELECT missync_recheck_generation FROM work_queue WHERE id = ?`, id).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	trip := func(answered bool) {
		t.Helper()
		flipped, err := q.MarkUpgradeQueued(ctx, []int64{id, fetch}, hold)
		if err != nil || !slices.Equal(flipped, []int64{id}) {
			t.Fatalf("flip = %v, %v; want only the post-settle row %d (a fetch-stamped row is never re-asked)", flipped, err, id)
		}
		if g := marker(); g.Valid {
			t.Fatalf("admission recorded the pass: %+v", g)
		}
		mustExec(t, dbh, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id)
		if ok, err := q.SettleUpgradeTrip(ctx, id, answered); err != nil || !ok {
			t.Fatalf("settle = %v, %v", ok, err)
		}
	}
	trip(false)
	if g := marker(); g.Valid {
		t.Fatalf("an unanswered trip recorded the pass: %+v", g)
	}
	if got, _ := q.ListUpgradeCandidates(ctx, hold, 10); len(got) != 0 {
		t.Fatalf("unanswered trip re-offered within the hold: %v", got)
	}
	if got, _ := q.ListUpgradeCandidates(ctx, afterHold, 10); !slices.Equal(got, []int64{id}) {
		t.Fatalf("unanswered trip after the hold = %v, want [%d]", got, id)
	}
	mustExec(t, dbh, `UPDATE work_queue SET upgrade_checked_at = NULL WHERE id = ?`, id)
	trip(true)
	if g := marker(); !g.Valid || g.Int64 != 9 {
		t.Fatalf("marker after an answered trip = %+v, want 9", g)
	}
	if got, _ := q.ListUpgradeCandidates(ctx, afterHold, 10); len(got) != 0 {
		t.Fatalf("passed row re-offered: %v", got)
	}
	q.SetProvidersVersion(10)
	if got, _ := q.ListUpgradeCandidates(ctx, afterHold, 10); !slices.Equal(got, []int64{id}) {
		t.Fatalf("after a lane-set change = %v, want [%d]", got, id)
	}
	q.SetProvidersVersion(9)
	// Stamps: a fetch stamp keeps the marker; a post-settle one clears it.
	rec := TimingRecord{Outcome: "mis_synced", EvaluatedAt: upgradeNow, Source: TimingSourceFetch}
	if err := q.SetTimingOutcome(ctx, id, rec); err != nil {
		t.Fatal(err)
	}
	if g := marker(); g.Int64 != 9 {
		t.Fatalf("fetch stamp changed the marker to %+v", g)
	}
	rec.Source = TimingSourceSweep
	if err := q.SetTimingOutcome(ctx, id, rec); err != nil {
		t.Fatal(err)
	}
	if g := marker(); g.Valid {
		t.Fatalf("sweep stamp left marker %+v", g)
	}
}

// TestTimingStampSource (#1120 review M3): each method records the source it
// is given; an empty one is NULL, which reads as post-settle (the row gets a
// provider pass, never skips one), and an unknown one is rejected.
func TestTimingStampSource(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	q.SetProvidersVersion(9)
	id := missyncedRow(t, dbh, "m", "missync_recheck_generation = 9")
	source := func() sql.NullString {
		var s sql.NullString
		if err := dbh.QueryRow(`SELECT timing_stamp_source FROM work_queue WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, src := range []string{TimingSourceFetch, TimingSourceSweep, TimingSourceRevalidate} {
		if err := q.SetTimingOutcome(ctx, id, TimingRecord{Outcome: "mis_synced", Source: src}); err != nil || source().String != src {
			t.Fatalf("SetTimingOutcome(%q) = %v, stored %+v", src, err, source())
		}
		if ok, err := q.SetTimingOutcomeIfIdle(ctx, id, TimingRecord{Outcome: "mis_synced", Source: src}); err != nil || !ok || source().String != src {
			t.Fatalf("SetTimingOutcomeIfIdle(%q) = %v, %v, stored %+v", src, ok, err, source())
		}
	}
	if err := q.SetTimingOutcome(ctx, id, TimingRecord{Outcome: "mis_synced"}); err != nil || source().Valid {
		t.Fatalf("empty source = %v, stored %+v; want NULL", err, source())
	}
	if ok, err := q.SetTimingOutcomeIfIdle(ctx, id, TimingRecord{Outcome: "mis_synced"}); err != nil || !ok || source().Valid {
		t.Fatalf("IfIdle empty source = %v, %v, stored %+v; want NULL", ok, err, source())
	}
	if got, _ := q.ListUpgradeCandidates(ctx, upgradeNow, 10); !slices.Equal(got, []int64{id}) {
		t.Fatalf("empty-source mis_synced row candidates = %v, want [%d] (fails toward a pass)", got, id)
	}
	if err := q.SetTimingOutcome(ctx, id, TimingRecord{Outcome: "ok", Source: "fetched"}); err == nil {
		t.Fatal("unknown source accepted")
	}
	if _, err := q.SetTimingOutcomeIfIdle(ctx, id, TimingRecord{Outcome: "ok", Source: "fetched"}); err == nil {
		t.Fatal("IfIdle unknown source accepted")
	}
}

// TestDequeueTierOrder (#1151): with one due row at each tier, the worker draws
// webhook, scan, the upgrade trip, then the deferred miss. One row per tier, so
// RANDOM() within a tier cannot affect the order.
func TestDequeueTierOrder(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	seed := func(key string, prio int, set string) {
		id := seedUpgradeRow(t, dbh, key, set)
		if _, err := dbh.Exec(`UPDATE work_queue SET status = 'pending', priority = ?, completed_at = NULL, next_attempt_at = ? WHERE id = ?`,
			prio, formatTime(upgradeNow.Add(-time.Minute)), id); err != nil {
			t.Fatal(err)
		}
	}
	// Insert lowest-first so insertion order cannot explain a pass.
	seed("miss", PriorityMiss, "")
	seed("upgrade", PriorityUpgrade, "upgrade_queued = 1")
	seed("scan", PriorityScan, "")
	seed("webhook", PriorityWebhook, "")
	for _, want := range []string{"webhook", "scan", "upgrade", "miss"} {
		item, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatalf("dequeue (want %s): %v", want, err)
		}
		if item.Inputs.Track.TrackName != want {
			t.Fatalf("dequeued %q, want %q", item.Inputs.Track.TrackName, want)
		}
	}
}

// TestEnqueueScanLiftsArmedUpgradeTrip (#1151): a scan enqueue colliding with a
// pending armed trip lifts it to scan priority and leaves it armed.
func TestEnqueueScanLiftsArmedUpgradeTrip(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	id := seedUpgradeRow(t, dbh, "t", "")
	if _, err := q.MarkUpgradeQueued(ctx, []int64{id}, upgradeNow.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	in := models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "t"}, SourcePath: "/m/x.flac", FromScan: true}
	if _, err := q.Enqueue(ctx, in, PriorityScan); err != nil {
		t.Fatal(err)
	}
	var prio, armed int
	var status string
	if err := dbh.QueryRow(`SELECT priority, upgrade_queued, status FROM work_queue WHERE id = ?`, id).Scan(&prio, &armed, &status); err != nil {
		t.Fatal(err)
	}
	if prio != PriorityScan || armed != 1 || status != StatusPending {
		t.Fatalf("after scan collision: priority=%d armed=%d status=%s; want %d, 1, pending", prio, armed, status, PriorityScan)
	}
}
