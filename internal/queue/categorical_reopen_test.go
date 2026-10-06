package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// TestLookupTimingDerivesJudgedSeconds (#972): the verdict stores no duration,
// but a measured stamp's overrun determines it (d = magnitude / (ratio - 1)).
// An unmeasured stamp has nothing to derive from and reports 0.
func TestLookupTimingDerivesJudgedSeconds(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	for _, tc := range []struct {
		title string
		rec   TimingRecord
		want  int
	}{
		{"measured", TimingRecord{Outcome: "categorical", Measured: true, Magnitude: 200, Ratio: 2.0}, 200},
		{"unmeasured", TimingRecord{Outcome: "categorical"}, 0},
		// A row that records a file or a hand edit is never reopened, so the
		// scan is given nothing to compare (no enqueue transaction per scan).
		{"hand-edited", TimingRecord{Outcome: "categorical", Measured: true, Magnitude: 200, Ratio: 2.0}, 0},
		{"file kept", TimingRecord{Outcome: "categorical", Measured: true, Magnitude: 200, Ratio: 2.0}, 0},
	} {
		item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: tc.title}}, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := q.SetTimingOutcome(ctx, item.ID, tc.rec); err != nil {
			t.Fatalf("SetTimingOutcome: %v", err)
		}
		if set := map[string]string{"hand-edited": "lyric_edited_at = '2026-02-01T00:00:00Z'",
			"file kept": "outcome_type = 'synced', sync_tier = 'line'"}[tc.title]; set != "" {
			if _, err := q.db.Exec(`UPDATE work_queue SET `+set+` WHERE id = ?`, item.ID); err != nil {
				t.Fatalf("setup %s: %v", tc.title, err)
			}
		}
		_, _, judged, found, err := q.LookupTiming(ctx, "A", tc.title)
		if err != nil || !found || judged != tc.want {
			t.Errorf("%s: judged = (%d, %v, %v); want %d", tc.title, judged, found, err, tc.want)
		}
	}
}

// seedCategoricalRow inserts recording A's settled row: done + categorical
// (judged 200 s), with A's paths, varied by status, outcome and the guards.
func seedCategoricalRow(t *testing.T, dbh *sql.DB, status, outcome string, upgradeQueued int, wordState any) int64 {
	t.Helper()
	var id int64
	if err := dbh.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, output_paths,
             status, timing_outcome, overrun_magnitude, overrun_ratio, evaluated_at, timing_stamp_source,
             refused_waits, upgrade_queued, word_timing_state, completed_at)
         VALUES ('A', 'T', 'a', 't', '/m/a.flac', '[{"outdir":"/m","filename":"a.lrc"}]', ?, ?, 200, 2.0,
             '2026-01-10T00:00:00Z', 'fetch', 2, ?, ?, '2026-01-10T00:00:00Z') RETURNING id`,
		status, outcome, upgradeQueued, wordState).Scan(&id); err != nil {
		t.Fatalf("seed work_queue: %v", err)
	}
	return id
}

func recordingBInputs(reopen bool) models.Inputs {
	return models.Inputs{
		Track:      models.Track{ArtistName: "A", TrackName: "T"},
		Outdir:     "/m",
		Filename:   "b.lrc",
		SourcePath: "/m/b.flac",
		FromScan:   true, ReopenCategorical: reopen,
	}
}

// TestEnqueueReopensCategoricalRowForDifferentRecording (#972): the reopen and
// the upsert run in one transaction, so the shared row moves to recording B and
// the worker claims B's paths with none of A's verdict left behind.
func TestEnqueueReopensCategoricalRowForDifferentRecording(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	id := seedCategoricalRow(t, dbh, "done", "categorical", 0, nil)

	if _, err := q.Enqueue(ctx, recordingBInputs(true), PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v; want the reopened row", err)
	}
	if item.ID != id || item.Inputs.SourcePath != "/m/b.flac" ||
		len(item.Inputs.OutputPaths) != 1 || item.Inputs.OutputPaths[0].Filename != "b.lrc" {
		t.Fatalf("dequeued id %d source %q outputs %+v; want row %d moved to recording B",
			item.ID, item.Inputs.SourcePath, item.Inputs.OutputPaths, id)
	}
	var verdict string
	if err := dbh.QueryRow(`SELECT COALESCE(timing_outcome, '') || '|' || COALESCE(overrun_magnitude, '') || '|' ||
            COALESCE(overrun_ratio, '') || '|' || COALESCE(evaluated_at, '') || '|' ||
            COALESCE(timing_stamp_source, '') || '|' || refused_waits FROM work_queue WHERE id = ?`, id).Scan(&verdict); err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	if verdict != "|||||0" {
		t.Fatalf("verdict columns %q; want all cleared -- A's verdict must not speak for B", verdict)
	}
}

// dumpCategoricalState renders every column of the row plus its junction
// links, so "refused" can be asserted as "nothing at all changed".
func dumpCategoricalState(t *testing.T, dbh *sql.DB, id int64) string {
	t.Helper()
	rows, err := dbh.Query(`SELECT * FROM work_queue WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("dump row: %v", err)
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if !rows.Next() {
		t.Fatalf("dump row %d: missing (%v)", id, rows.Err())
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("dump row scan: %v", err)
	}
	_ = rows.Close() // the test DB has one connection: release it before the next read
	var links string
	if err := dbh.QueryRow(`SELECT COALESCE(group_concat(scan_result_id), '') FROM work_queue_scan_results
            WHERE work_queue_id = ?`, id).Scan(&links); err != nil {
		t.Fatalf("dump links: %v", err)
	}
	return fmt.Sprintf("%v links=%s", vals, links)
}

// TestEnqueueReopenCategoricalIsDecidedInTheTransaction (#972): the scan sets
// ReopenCategorical from a read outside the enqueue transaction, so Enqueue
// judges the row again. A categorical row it cannot reopen refuses with
// ErrCategoricalNotReopened and changes nothing; a settled link, or the
// incoming file's own unfinished link, does not block. Unasked, not
// categorical, or pending/failed/deferred outside a trip, the row collides as
// an ordinary enqueue does.
func TestEnqueueReopenCategoricalIsDecidedInTheTransaction(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, status, outcome string
		upgrade               int
		wordState             any
		setup                 string // extra SET clause for the seeded row
		otherLink             string // status of recording A's linked scan row; "" = none
		ownLinked             bool   // B's own unfinished scan row is already linked
		reopen, refused       bool
		wantRow               string
	}{
		{name: "processing", status: "processing", reopen: true, refused: true},
		{name: "unavailable", status: "unavailable", reopen: true, refused: true},
		{name: "upgrade trip", status: "done", upgrade: 1, reopen: true, refused: true},
		{name: "pending in an upgrade trip", status: "pending", upgrade: 1, reopen: true, refused: true},
		// done + queued is what prune's retire leaves; not a live recheck.
		{name: "retired over a queued word recheck", status: "done", wordState: "queued", reopen: true, refused: true},
		// The live recheck shape: reopenWordRecheckForScan reopens it before the
		// categorical guard runs, so it moves to B like any scan collision.
		{name: "parked word recheck", status: "deferred", wordState: "queued", reopen: true, wantRow: "pending|/m/b.flac|"},
		{name: "linked to an unfinished file", status: "done", otherLink: "processing", reopen: true, refused: true},
		// Pins the known limit tracked in #1366: the judged recording's own scan
		// result was re-pended (forced scan, generation change) and is still
		// linked, so B is refused until it settles. Change this deliberately.
		{name: "judged recording re-pended", status: "done", otherLink: "pending", reopen: true, refused: true},
		{name: "linked to a settled file", status: "done", otherLink: "done", reopen: true, wantRow: "pending|/m/b.flac|"},
		{name: "own unfinished link", status: "done", ownLinked: true, reopen: true, wantRow: "pending|/m/b.flac|"},
		{name: "hand-edited", status: "done", setup: "lyric_edited_at = '2026-02-01T00:00:00Z'", reopen: true, refused: true},
		{name: "file kept by the sweep", status: "done", setup: "outcome_type = 'synced'", reopen: true, refused: true},
		{name: "tier recorded", status: "done", setup: "sync_tier = 'line'", reopen: true, refused: true},
		{name: "pending outside a trip", status: "pending", reopen: true, wantRow: "pending|/m/b.flac|categorical"},
		{name: "failed outside a trip", status: "failed", reopen: true, wantRow: "failed|/m/b.flac|categorical"},
		{name: "deferred outside a trip", status: "deferred", reopen: true, wantRow: "deferred|/m/b.flac|categorical"},
		{name: "not categorical", status: "done", outcome: "mis_synced", reopen: true, wantRow: "done|/m/a.flac|mis_synced"},
		{name: "flag unset", status: "done", wantRow: "done|/m/a.flac|categorical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.outcome == "" {
				tc.outcome = "categorical"
			}
			dbh := openQueueTestDB(t)
			id := seedCategoricalRow(t, dbh, tc.status, tc.outcome, tc.upgrade, tc.wordState)
			if tc.setup != "" {
				if _, err := dbh.Exec(`UPDATE work_queue SET `+tc.setup+` WHERE id = ?`, id); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			libID, other := insertLibraryAndScanResult(t, dbh, "/m", "/m/a.flac")
			if tc.otherLink != "" {
				linkScanResult(t, dbh, id, other)
				if _, err := dbh.Exec(`UPDATE scan_results SET status = ? WHERE id = ?`, tc.otherLink, other); err != nil {
					t.Fatalf("set other scan row status: %v", err)
				}
			}
			inputs := recordingBInputs(tc.reopen)
			if err := dbh.QueryRow(`INSERT INTO scan_results (library_id, artist, title, file_path, outdir, filename, status)
                    VALUES (?, 'A', 'T', '/m/b.flac', '/m', 'b.lrc', 'processing') RETURNING id`, libID).Scan(&inputs.ScanResultID); err != nil {
				t.Fatalf("insert B scan row: %v", err)
			}
			if tc.ownLinked {
				linkScanResult(t, dbh, id, inputs.ScanResultID)
			}
			before := dumpCategoricalState(t, dbh, id)

			_, err := NewDBQueue(dbh).Enqueue(ctx, inputs, PriorityScan)
			if errors.Is(err, ErrCategoricalNotReopened) != tc.refused || (err != nil && !tc.refused) {
				t.Fatalf("Enqueue err = %v; want refused = %v", err, tc.refused)
			}
			if tc.refused {
				if after := dumpCategoricalState(t, dbh, id); after != before {
					t.Fatalf("refused enqueue changed the row or its links:\n before %s\n after  %s", before, after)
				}
				return
			}
			var got string
			var links int
			if err := dbh.QueryRow(`SELECT status || '|' || source_path || '|' || COALESCE(timing_outcome, ''),
                    (SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = work_queue.id AND scan_result_id = ?)
                    FROM work_queue WHERE id = ?`, inputs.ScanResultID, id).Scan(&got, &links); err != nil {
				t.Fatalf("read: %v", err)
			}
			if got != tc.wantRow || links != 1 {
				t.Fatalf("row = %q, B links = %d; want %q linked once", got, links, tc.wantRow)
			}
		})
	}
}

// TestEnqueueReopenCategoricalReturnsARealErrorAsItself (#972): a database
// error from the reopen is not the "not reopened" sentinel, which the scan
// counts and skips. The failure is forced with a real trigger on the reopen's
// own UPDATE.
func TestEnqueueReopenCategoricalReturnsARealErrorAsItself(t *testing.T) {
	dbh := openQueueTestDB(t)
	seedCategoricalRow(t, dbh, "done", "categorical", 0, nil)
	if _, err := dbh.Exec(`CREATE TRIGGER force_reopen_failure BEFORE UPDATE OF status ON work_queue
            WHEN OLD.status = 'done' AND NEW.status = 'pending'
            BEGIN SELECT RAISE(ABORT, 'forced reopen failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	_, err := NewDBQueue(dbh).Enqueue(context.Background(), recordingBInputs(true), PriorityScan)
	if err == nil || !strings.Contains(err.Error(), "forced reopen failure") || errors.Is(err, ErrCategoricalNotReopened) {
		t.Fatalf("Enqueue err = %v; want the database error itself, never ErrCategoricalNotReopened", err)
	}
}
