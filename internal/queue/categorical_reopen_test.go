package queue

import (
	"context"
	"database/sql"
	"errors"
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
	} {
		item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: tc.title}}, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := q.SetTimingOutcome(ctx, item.ID, tc.rec); err != nil {
			t.Fatalf("SetTimingOutcome: %v", err)
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

// TestEnqueueLeavesOtherRowsAlone (#972): only a done + categorical row outside
// an upgrade trip or word recheck is reopened, and only when asked; any other
// row collides as before and keeps recording A.
func TestEnqueueLeavesOtherRowsAlone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, status, outcome string
		upgrade               int
		wordState             any
		reopen                bool
	}{
		{"processing", "processing", "categorical", 0, nil, true},
		{"upgrade trip", "done", "categorical", 1, nil, true},
		{"word recheck queued", "done", "categorical", 0, "queued", true},
		{"not categorical", "done", "mis_synced", 0, nil, true},
		{"flag unset", "done", "categorical", 0, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbh := openQueueTestDB(t)
			id := seedCategoricalRow(t, dbh, tc.status, tc.outcome, tc.upgrade, tc.wordState)
			if _, err := NewDBQueue(dbh).Enqueue(ctx, recordingBInputs(tc.reopen), PriorityScan); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			var got string
			if err := dbh.QueryRow(`SELECT status || '|' || source_path || '|' || COALESCE(timing_outcome, '')
                    FROM work_queue WHERE id = ?`, id).Scan(&got); err != nil {
				t.Fatalf("read: %v", err)
			}
			if want := tc.status + "|/m/a.flac|" + tc.outcome; got != want {
				t.Fatalf("row = %q; want %q untouched", got, want)
			}
			if _, err := NewDBQueue(dbh).Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("Dequeue err = %v; want sql.ErrNoRows, nothing reopened", err)
			}
		})
	}
}
