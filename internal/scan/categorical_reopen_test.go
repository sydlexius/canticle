package scan_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/timing"
)

// TestEnqueuePendingReopensCategoricalOnlyForDifferentRecording (#972): a
// categorical verdict is keyed by artist and title, so it also covers another
// recording of the song. Only a file whose known duration differs from the
// judged one by MORE than timing.Tolerance is enqueued (asking Enqueue to
// reopen the row); every case short of that proof keeps the suppression.
func TestEnqueuePendingReopensCategoricalOnlyForDifferentRecording(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		judged       int
		fileSeconds  int // 0 = no duration recorded (unknown)
		verdictGen   int
		wantEnqueued bool
	}{
		{"different length", 200, 300, 3, true},
		{"different length, older generation", 200, 300, 2, true},
		{"same length", 200, 201, 3, false},
		{"exactly Tolerance apart", 200, 200 + int(timing.Tolerance), 3, false},
		{"unknown duration", 200, 0, 3, false},
		{"underivable verdict", 0, 300, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			durations := audiodur.New(openTestDB(t), "test-reader-v1")
			path := writeTestAudioFile(t, t.TempDir(), "b.flac")
			if tc.fileSeconds > 0 {
				recordRealDuration(ctx, t, durations, path, tc.fileSeconds)
			}
			store := &fakePendingStore{results: []models.ScanResult{{ID: 1, FilePath: path,
				Track: models.Track{ArtistName: "A", TrackName: "T"}}}}
			verdicts := &fakeTimingVerdicts{verdicts: map[string]scan.TimingVerdict{
				"A\x00T": {Outcome: timing.Categorical, ProvidersVersion: tc.verdictGen, JudgedSeconds: tc.judged},
			}}
			work := &fakeWorkQueue{}
			e := scan.Enqueuer{Results: store, Cache: fakeLyricsCache{}, Queue: work,
				Timing: verdicts, ProvidersVersion: 3, Durations: durations}

			if _, _, err := e.EnqueuePending(ctx, models.Library{ID: 7}); err != nil {
				t.Fatalf("EnqueuePending: %v", err)
			}
			if !tc.wantEnqueued {
				if len(work.inputs) != 0 {
					t.Fatalf("enqueued %d; want the categorical suppression kept", len(work.inputs))
				}
				return
			}
			if len(work.inputs) != 1 || !work.inputs[0].ReopenCategorical || !work.inputs[0].FromScan {
				t.Fatalf("inputs = %+v; want one scan enqueue asking to reopen the categorical row", work.inputs)
			}
		})
	}
}

// TestScanReopensCategoricalRowEndToEnd (#972) runs the probe shape on a real
// database: recording A settled done + categorical (judged 200 s, nothing
// written), recording B of the same song pending. A different-length B moves
// the shared row to itself and is what the worker claims, its scan row linked;
// a same-length B stays pending with nothing to claim.
func TestScanReopensCategoricalRowEndToEnd(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		fileSeconds int
		wantClaimB  bool
	}{
		{"different length", 300, true},
		{"same length", 201, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbh := openTestDB(t)
			root := t.TempDir()
			lib, err := library.New(dbh).Add(ctx, root, "M", models.LibrarySettings{})
			if err != nil {
				t.Fatalf("add library: %v", err)
			}
			pathB := writeTestAudioFile(t, root, "b.flac")
			durations := audiodur.New(dbh, "test-reader-v1")
			recordRealDuration(ctx, t, durations, pathB, tc.fileSeconds)
			repo := scan.New(dbh)
			if err := repo.Upsert(ctx, lib.ID, []models.ScanResult{{FilePath: pathB,
				Track: models.Track{ArtistName: "A", TrackName: "T"}, Outdir: root, Filename: "b.lrc"}}, scan.UpsertOptions{}); err != nil {
				t.Fatalf("upsert: %v", err)
			}
			pathA := filepath.Join(root, "a.flac")
			var rowID int64
			if err := dbh.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, output_paths,
                     status, providers_version, timing_outcome, overrun_magnitude, overrun_ratio, completed_at)
                 VALUES ('A', 'T', 'a', 't', ?, ?, 'done', 1, 'categorical', 200, 2.0, '2026-01-10T00:00:00Z') RETURNING id`,
				pathA, `[{"outdir":"`+root+`","filename":"a.lrc"}]`).Scan(&rowID); err != nil {
				t.Fatalf("seed work_queue: %v", err)
			}
			q := queue.NewDBQueue(dbh)
			q.SetProvidersVersion(1)
			e := scan.Enqueuer{Results: repo, Cache: cache.New(dbh), Queue: q, Priority: queue.PriorityScan,
				Timing: scan.TimingVerdicts{Reader: q}, ProvidersVersion: 1, Durations: durations}
			if _, _, err := e.EnqueuePending(ctx, lib); err != nil {
				t.Fatalf("EnqueuePending: %v", err)
			}

			var bStatus string
			var links int
			if err := dbh.QueryRow(`SELECT sr.status, (SELECT COUNT(*) FROM work_queue_scan_results j
                     WHERE j.work_queue_id = ? AND j.scan_result_id = sr.id)
                 FROM scan_results sr WHERE sr.file_path = ?`, rowID, pathB).Scan(&bStatus, &links); err != nil {
				t.Fatalf("read B scan row: %v", err)
			}
			item, err := q.Dequeue(ctx)
			if !tc.wantClaimB {
				if !errors.Is(err, sql.ErrNoRows) || bStatus != scan.StatusPending || links != 0 {
					t.Fatalf("dequeue err %v, B status %q, links %d; want nothing claimed and B left pending",
						err, bStatus, links)
				}
				return
			}
			if err != nil || item.ID != rowID || item.Inputs.SourcePath != pathB {
				t.Fatalf("dequeued (%d, %q, %v); want row %d claimed for recording B", item.ID, item.Inputs.SourcePath, err, rowID)
			}
			if bStatus != scan.StatusProcessing || links != 1 {
				t.Fatalf("B status %q, links %d; want B reserved and linked to the row", bStatus, links)
			}
		})
	}
}

// TestScanLeavesFilePendingWhileWorkerHoldsCategoricalRow (#972): the verdict
// read ignores status, so a row a worker still holds ('processing', stamped
// categorical before Complete) asks for a reopen that cannot happen. The file
// must not be linked to that row: its Complete would mark the file done though
// it was never fetched. It stays pending, and the next scan, with the row
// settled, reopens the row for it.
func TestScanLeavesFilePendingWhileWorkerHoldsCategoricalRow(t *testing.T) {
	ctx := context.Background()
	dbh := openTestDB(t)
	root := t.TempDir()
	lib, err := library.New(dbh).Add(ctx, root, "M", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("add library: %v", err)
	}
	pathB := writeTestAudioFile(t, root, "b.flac")
	durations := audiodur.New(dbh, "test-reader-v1")
	recordRealDuration(ctx, t, durations, pathB, 300)
	repo := scan.New(dbh)
	if err := repo.Upsert(ctx, lib.ID, []models.ScanResult{{FilePath: pathB,
		Track: models.Track{ArtistName: "A", TrackName: "T"}, Outdir: root, Filename: "b.lrc"}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var rowID int64
	if err := dbh.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, output_paths,
             status, providers_version, timing_outcome, overrun_magnitude, overrun_ratio)
         VALUES ('A', 'T', 'a', 't', ?, ?, 'processing', 1, 'categorical', 200, 2.0) RETURNING id`,
		filepath.Join(root, "a.flac"), `[{"outdir":"`+root+`","filename":"a.lrc"}]`).Scan(&rowID); err != nil {
		t.Fatalf("seed work_queue: %v", err)
	}
	q := queue.NewDBQueue(dbh)
	q.SetProvidersVersion(1)
	e := scan.Enqueuer{Results: repo, Cache: cache.New(dbh), Queue: q, Priority: queue.PriorityScan,
		Timing: scan.TimingVerdicts{Reader: q}, ProvidersVersion: 1, Durations: durations}

	state := func() (status string, links int) {
		t.Helper()
		if err := dbh.QueryRow(`SELECT sr.status, (SELECT COUNT(*) FROM work_queue_scan_results j
                 WHERE j.scan_result_id = sr.id) FROM scan_results sr WHERE sr.file_path = ?`, pathB).Scan(&status, &links); err != nil {
			t.Fatalf("read B scan row: %v", err)
		}
		return status, links
	}
	enqueued, _, err := e.EnqueuePending(ctx, lib)
	if err != nil || enqueued != 0 {
		t.Fatalf("EnqueuePending = (%d, %v); want nothing enqueued and no error", enqueued, err)
	}
	if status, links := state(); status != scan.StatusPending || links != 0 {
		t.Fatalf("B status %q, links %d; want pending and unlinked while the worker holds the row", status, links)
	}

	if err := q.Complete(ctx, rowID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if status, _ := state(); status != scan.StatusPending {
		t.Fatalf("B status %q after the worker's Complete; want pending, never marked done unfetched", status)
	}
	if enqueued, _, err := e.EnqueuePending(ctx, lib); err != nil || enqueued != 1 {
		t.Fatalf("second EnqueuePending = (%d, %v); want B enqueued once the row settled", enqueued, err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil || item.ID != rowID || item.Inputs.SourcePath != pathB {
		t.Fatalf("dequeued (%d, %q, %v); want row %d claimed for recording B", item.ID, item.Inputs.SourcePath, err, rowID)
	}
}
