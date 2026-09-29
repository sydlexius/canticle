package worker

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

// TestWorker_KeepsBetterLyricsOnDisk is the #553 data-loss path end to end:
// a reopened row whose settled .lrc is better than the new result (a plain
// .txt, or a detector instrumental verdict) keeps the .lrc byte-for-byte, and
// the row completes still describing that .lrc rather than the refused result.
func TestWorker_KeepsBetterLyricsOnDisk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		primary *fakeFetcher
		setup   func(*Worker)
	}{
		{"unsynced result", &fakeFetcher{song: models.Song{Track: models.Track{ArtistName: "Synthetic Artist"},
			Lyrics: models.Lyrics{LyricsBody: "plain words"}}}, func(*Worker) {}},
		{"detector instrumental", &fakeFetcher{err: musixmatch.ErrNotFound}, func(w *Worker) {
			w.SetFallbackProviders()
			w.EnableAudioDetector(&fakeDetector{instrumental: true, version: "9.9.9"})
			w.SetInstrumentalDetectionDefault(true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, w := newRecheckRig(t, tc.primary, nil, true)
			ctx := context.Background()
			if err := rig.q.SetSyncTier(ctx, rig.id, queue.SyncTierLine); err != nil {
				t.Fatal(err)
			}
			if _, err := rig.db.Exec(`UPDATE work_queue SET status = 'pending' WHERE id = ?`, rig.id); err != nil {
				t.Fatal(err)
			}
			tc.setup(w)
			if err := w.RunOnce(ctx); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if got, err := os.ReadFile(rig.lrc); err != nil || !bytes.Equal(got, rig.original) {
				t.Fatalf(".lrc changed or removed: %q, %v", got, err)
			}
			if _, err := os.Stat(strings.TrimSuffix(rig.lrc, ".lrc") + ".txt"); !os.IsNotExist(err) {
				t.Errorf("a .txt landed beside the kept .lrc: %v", err)
			}
			var status, outcome, tier string
			var instrumental sql.NullInt64
			if err := rig.db.QueryRow(`SELECT status, COALESCE(outcome_type, ''), COALESCE(sync_tier, ''), instrumental_result
				FROM work_queue WHERE id = ?`, rig.id).Scan(&status, &outcome, &tier, &instrumental); err != nil {
				t.Fatal(err)
			}
			if status != queue.StatusDone || outcome != "synced" || tier != queue.SyncTierLine || instrumental.Valid {
				t.Errorf("row = %s/%q/%q/instrumental=%v; want done, synced, line, no verdict", status, outcome, tier, instrumental)
			}
		})
	}
}

// TestWorker_KeptRowStampsFromDiskNotTheRefusedResult covers the two ways a
// kept completion used to describe the refused result instead of the kept file
// (#553 review): the refused lane was stamped onto provider_lane (so
// purgeprovenance.provenanceAgrees misattributed the kept file) and the refused
// result was cached; and after queue.ReopenDoneRowTx cleared the settle
// columns, the kept row settled done with outcome_type NULL (the #655 state).
func TestWorker_KeptRowStampsFromDiskNotTheRefusedResult(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(map[bool]string{false: "lane of the kept file survives", true: "after ReopenDoneRowTx"}[reopen], func(t *testing.T) {
			primary := &fakeFetcher{song: models.Song{Track: models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"},
				Lyrics: models.Lyrics{LyricsBody: "plain words"}}}
			rig, w := newRecheckRig(t, primary, nil, true)
			ctx := context.Background()
			// The kept .lrc was written by the petitlyrics lane; the refused
			// result comes from musixmatch.
			if _, err := rig.db.Exec(`UPDATE work_queue SET provider_lane = 'petitlyrics', sync_tier = NULL WHERE id = ?`, rig.id); err != nil {
				t.Fatal(err)
			}
			wantLane := "petitlyrics"
			if reopen {
				tx, err := rig.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := queue.ReopenDoneRowTx(ctx, tx, rig.id, time.Now()); err != nil || !ok {
					t.Fatalf("reopen = %v, %v", ok, err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				wantLane = ""
			} else if _, err := rig.db.Exec(`UPDATE work_queue SET status = 'pending' WHERE id = ?`, rig.id); err != nil {
				t.Fatal(err)
			}
			if err := w.RunOnce(ctx); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if primary.calls == 0 {
				t.Fatal("the provider was never asked; the kept path was not reached")
			}
			if got, err := os.ReadFile(rig.lrc); err != nil || !bytes.Equal(got, rig.original) {
				t.Fatalf(".lrc changed or removed: %q, %v", got, err)
			}
			var status, outcome, tier, lane string
			if err := rig.db.QueryRow(`SELECT status, COALESCE(outcome_type, ''), COALESCE(sync_tier, ''), COALESCE(provider_lane, '')
				FROM work_queue WHERE id = ?`, rig.id).Scan(&status, &outcome, &tier, &lane); err != nil {
				t.Fatal(err)
			}
			if status != queue.StatusDone || outcome != "synced" || tier != queue.SyncTierLine {
				t.Errorf("row = %s/%q/%q; want done, synced, line (the kept file)", status, outcome, tier)
			}
			if lane != wantLane {
				t.Errorf("provider_lane = %q; want %q (never the refused lane)", lane, wantLane)
			}
			var cached int
			if err := rig.db.QueryRow(`SELECT COUNT(*) FROM lyrics_cache`).Scan(&cached); err != nil {
				t.Fatal(err)
			}
			if cached != 0 {
				t.Errorf("lyrics_cache rows = %d; a refused result must not be cached", cached)
			}
		})
	}
}
