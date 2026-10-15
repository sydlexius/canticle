package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
)

// goneEntry is a single output entry whose directory never existed.
func goneEntry(_, root, _ string) []models.OutputPath {
	return []models.OutputPath{{Outdir: filepath.Join(root, "artist", "Gone"), Filename: "01.lrc"}}
}

// settleAsUnsynced puts the rig's row where a long-settled unsynced row sits:
// done, completed long ago, never admitted by the upgrade sweep.
func (r *healRig) settleAsUnsynced(t *testing.T) {
	t.Helper()
	if _, err := r.db.Exec(`UPDATE work_queue SET status = 'done', outcome_type = 'unsynced', last_error = '',
            completed_at = '2026-01-01T00:00:00Z' WHERE id = ?`, r.id); err != nil {
		t.Fatal(err)
	}
}

func (r *healRig) retireCols(t *testing.T) (status, lastErr string, upgradeQueued int) {
	t.Helper()
	if err := r.db.QueryRow(`SELECT status, last_error, upgrade_queued FROM work_queue WHERE id = ?`, r.id).
		Scan(&status, &lastErr, &upgradeQueued); err != nil {
		t.Fatal(err)
	}
	return status, lastErr, upgradeQueued
}

// upgradeListed reports whether the upgrade sweep would admit the row now,
// with the hold cut far in the future so only the predicate decides.
func (r *healRig) upgradeListed(t *testing.T) bool {
	t.Helper()
	ids, err := r.q.ListUpgradeCandidates(context.Background(), time.Now().Add(365*24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(ids, r.id)
}

func (r *healRig) goneMark(t *testing.T) (mark sql.NullString, misses int) {
	t.Helper()
	if err := r.db.QueryRow(`SELECT source_gone_at, upgrade_miss_count FROM work_queue WHERE id = ?`, r.id).Scan(&mark, &misses); err != nil {
		t.Fatal(err)
	}
	return mark, misses
}

// trip flips the settled row into an upgrade trip and runs the worker once.
func (r *healRig) trip(t *testing.T) {
	t.Helper()
	if flipped, err := r.q.MarkUpgradeQueued(context.Background(), []int64{r.id}, time.Now().Add(365*24*time.Hour)); err != nil || len(flipped) != 1 {
		t.Fatalf("MarkUpgradeQueued = %v, %v; want the row flipped", flipped, err)
	}
	if err := r.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

// goneRig is a settled unsynced row whose whole artist folder is removed, with
// the root kept populated by a sibling folder so it still reads online.
func goneRig(t *testing.T, paths func(srcDir, root, rootB string) []models.OutputPath) *healRig {
	t.Helper()
	rig := newLinkedRig(t, paths)
	mkSource(t, filepath.Join(rig.root, "keep"))
	rig.settleAsUnsynced(t)
	if err := os.RemoveAll(filepath.Join(rig.root, "artist")); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (r *healRig) backdateMark(t *testing.T, age time.Duration) {
	t.Helper()
	if _, err := r.db.Exec(`UPDATE work_queue SET source_gone_at = ? WHERE id = ?`,
		time.Now().Add(-age).UTC().Format(time.RFC3339), r.id); err != nil {
		t.Fatal(err)
	}
}

// #1597: the FIRST upgrade trip that finds the source provably gone does not
// retire. It settles as an answered miss (the hold escalates), records the
// sighting and fetches nothing; the row stays an upgrade candidate.
func TestRunOnce_FirstGoneSightingMarksAndEscalatesButDoesNotRetire(t *testing.T) {
	rig := goneRig(t, goneEntry)
	rig.trip(t)
	status, lastErr, armed := rig.retireCols(t)
	mark, misses := rig.goneMark(t)
	if status != queue.StatusDone || lastErr != "" || armed != 0 || !mark.Valid || misses != 1 || rig.fetcher.calls != 0 {
		t.Errorf("status=%s last_error=%q armed=%d mark=%v misses=%d fetches=%d; want done, empty, 0, marked, 1, no fetch",
			status, lastErr, armed, mark, misses, rig.fetcher.calls)
	}
	if !rig.upgradeListed(t) {
		t.Error("a marked row must stay an upgrade candidate until a second sighting")
	}
}

// A LATER trip that again finds it gone, with the mark at least 24h old,
// retires the row without a fetch, and the sweep never admits it again.
func TestRunOnce_SecondGoneSightingAfter24hRetires(t *testing.T) {
	rig := goneRig(t, goneEntry)
	srID := linkScan(t, rig.db, rig.libA, rig.id, filepath.Join(rig.srcDir, "02.flac"))
	if _, err := rig.db.Exec(`UPDATE scan_results SET status = 'processing' WHERE id = ?`, srID); err != nil {
		t.Fatal(err)
	}
	rig.trip(t)
	rig.backdateMark(t, 25*time.Hour)
	rig.trip(t)
	status, lastErr, armed := rig.retireCols(t)
	if status != queue.StatusDone || lastErr != queue.UnresolvableGoneError || armed != 0 || rig.fetcher.calls != 0 {
		t.Errorf("status=%s last_error=%q armed=%d fetches=%d; want done, the gone sentinel, 0, no fetch", status, lastErr, armed, rig.fetcher.calls)
	}
	if rig.upgradeListed(t) {
		t.Error("the retired row is still an upgrade candidate; the sweep would re-admit it")
	}
	if st := rig.scanStatus(t, srID); st != "done" {
		t.Errorf("scan_results status = %s; want done", st)
	}
}

// A second sighting inside 24h is not a confirmation.
func TestRunOnce_SecondGoneSightingBefore24hDoesNotRetire(t *testing.T) {
	rig := goneRig(t, goneEntry)
	rig.trip(t)
	rig.backdateMark(t, 23*time.Hour)
	rig.trip(t)
	if _, lastErr, _ := rig.retireCols(t); lastErr == queue.UnresolvableGoneError {
		t.Errorf("last_error = the gone sentinel; a 23h-old mark must not confirm")
	}
}

// The file (or its output directory) back on a later trip clears the mark, so
// the next gone sighting starts over instead of retiring.
func TestRunOnce_FileBackClearsTheGoneMark(t *testing.T) {
	twoGone := func(_, root, _ string) []models.OutputPath {
		return []models.OutputPath{
			{Outdir: filepath.Join(root, "artist", "Gone1"), Filename: "a.lrc"},
			{Outdir: filepath.Join(root, "artist", "Gone2"), Filename: "b.lrc"},
		}
	}
	for _, tc := range []struct {
		name  string
		paths func(srcDir, root, rootB string) []models.OutputPath
		back  func(t *testing.T, rig *healRig)
	}{
		// Source present, outputs gone, two entries so no heal: preflight fails and
		// the present source drops the mark.
		{"source-back-outputs-gone", twoGone, func(t *testing.T, rig *healRig) { mkSource(t, rig.srcDir) }},
		// Outputs back: the preflight passes and the trip drops the mark.
		{"output-dir-back", goneEntry, func(t *testing.T, rig *healRig) {
			mkSource(t, rig.srcDir)
			if err := os.MkdirAll(filepath.Join(rig.root, "artist", "Gone"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := goneRig(t, tc.paths)
			rig.trip(t)
			if mark, _ := rig.goneMark(t); !mark.Valid {
				t.Fatal("control: the first sighting did not mark the row")
			}
			tc.back(t, rig)
			rig.trip(t)
			if mark, _ := rig.goneMark(t); mark.Valid {
				t.Errorf("mark = %q after the file came back; want cleared", mark.String)
			}
			if _, lastErr, _ := rig.retireCols(t); lastErr == queue.UnresolvableGoneError {
				t.Error("a row whose file came back was retired")
			}
		})
	}
}

// Ordinary claims keep #1430's fail-into-backoff: no retire, no mark.
func TestRunOnce_OrdinaryClaimWithSourceGoneStillFails(t *testing.T) {
	rig := newLinkedRig(t, goneEntry)
	mkSource(t, filepath.Join(rig.root, "keep"))
	if err := os.RemoveAll(filepath.Join(rig.root, "artist")); err != nil {
		t.Fatal(err)
	}
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, lastErr, _ := rig.retireCols(t)
	if mark, _ := rig.goneMark(t); status != "failed" || lastErr == queue.UnresolvableGoneError || mark.Valid || rig.fetcher.calls != 0 {
		t.Errorf("status=%s last_error=%q mark=%v fetches=%d; want failed, not retired, unmarked, no fetch", status, lastErr, mark, rig.fetcher.calls)
	}
}

// Every doubt keeps today's behavior on an upgrade trip: failed, unmarked.
func TestRunOnce_UnprovenGoneDoesNotMarkOrRetire(t *testing.T) {
	twoGone := func(_, root, _ string) []models.OutputPath {
		return []models.OutputPath{
			{Outdir: filepath.Join(root, "artist", "Gone1"), Filename: "a.lrc"},
			{Outdir: filepath.Join(root, "artist", "Gone2"), Filename: "b.lrc"},
		}
	}
	for _, tc := range []struct {
		name  string
		paths func(srcDir, root, rootB string) []models.OutputPath
		doubt func(t *testing.T, rig *healRig)
	}{
		// The mount drops inside the worker's cached-online window: its cached
		// answer still says online, a fresh probe reads the empty root as offline.
		{"mount-dropped-inside-cached-online-window", goneEntry, func(t *testing.T, rig *healRig) {
			rig.w.rootIsOnline(rig.root) // prime the cache while the root is populated
			if err := os.RemoveAll(rig.root); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(rig.root, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		// The source folder is removed but its parent is left empty, as an
		// unmounted sub-share reads: not a removal.
		{"empty-sub-mountpoint", goneEntry, func(t *testing.T, rig *healRig) {
			if err := os.MkdirAll(filepath.Join(rig.root, "artist"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		// A stat error other than not-exist (EACCES): readable but unsearchable.
		{"stat-error-is-not-gone", twoGone, func(t *testing.T, rig *healRig) {
			mkSource(t, rig.srcDir)
			if err := os.Chmod(rig.srcDir, 0o444); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(rig.srcDir, 0o755) })
			if _, err := os.Lstat(filepath.Join(rig.srcDir, "01.flac")); err == nil {
				t.Skip("directory permissions are not enforced here (running as root?)")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newLinkedRig(t, tc.paths)
			mkSource(t, filepath.Join(rig.root, "keep"))
			rig.settleAsUnsynced(t)
			if tc.name != "stat-error-is-not-gone" {
				if err := os.RemoveAll(filepath.Join(rig.root, "artist")); err != nil {
					t.Fatal(err)
				}
			}
			tc.doubt(t, rig)
			rig.trip(t)
			mark, _ := rig.goneMark(t)
			status, lastErr, _ := rig.retireCols(t)
			if mark.Valid || lastErr == queue.UnresolvableGoneError || status != "failed" || rig.fetcher.calls != 0 {
				t.Errorf("status=%s last_error=%q mark=%v fetches=%d; want failed, not retired, unmarked, no fetch", status, lastErr, mark, rig.fetcher.calls)
			}
		})
	}
}

// failGoneQueue fails SettleGoneUpgradeTrip, as a locked database would.
type failGoneQueue struct{ *queue.DBQueue }

func (failGoneQueue) SettleGoneUpgradeTrip(context.Context, int64, time.Duration) (queue.GoneOutcome, error) {
	return queue.GoneNotTrip, errors.New("database is locked")
}

// A settle error falls back to the ordinary fail, never silently swallowing the row.
func TestRunOnce_GoneSettleErrorFallsBackToFail(t *testing.T) {
	rig := goneRig(t, goneEntry)
	rig.w = New(failGoneQueue{rig.q}, cache.New(rig.db), rig.fetcher, lyrics.NewLRCWriter(rig.root, rig.rootB))
	rig.w.SetMetadataReader((&fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: fallthroughFileSeconds}}).read)
	rig.w.SetOutputHealer(rig.healer)
	rig.trip(t)
	status, lastErr, _ := rig.retireCols(t)
	if mark, _ := rig.goneMark(t); status != "failed" || mark.Valid || lastErr == "" || lastErr == queue.UnresolvableGoneError {
		t.Errorf("status=%s last_error=%q mark=%v; want failed with the settle error recorded", status, lastErr, mark)
	}
}

// #1597 boundary: with the source present and only the output directory gone,
// an upgrade trip keeps today's behavior: it fails (two entries, so no heal is
// attempted) and is not retired.
func TestRunOnce_UpgradeTripWithSourcePresentIsNotRetired(t *testing.T) {
	rig := newLinkedRig(t, func(_, root, _ string) []models.OutputPath {
		return []models.OutputPath{
			{Outdir: filepath.Join(root, "artist", "Gone1"), Filename: "a.lrc"},
			{Outdir: filepath.Join(root, "artist", "Gone2"), Filename: "b.lrc"},
		}
	})
	rig.settleAsUnsynced(t)
	if flipped, err := rig.q.MarkUpgradeQueued(context.Background(), []int64{rig.id}, time.Now().Add(365*24*time.Hour)); err != nil || len(flipped) != 1 {
		t.Fatalf("MarkUpgradeQueued = %v, %v; want the row flipped", flipped, err)
	}
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, lastErr, armed := rig.retireCols(t)
	if status != "failed" || lastErr == queue.UnresolvableGoneError || armed != 1 || rig.fetcher.calls != 0 {
		t.Errorf("status=%s last_error=%q upgrade_queued=%d fetches=%d; want failed, not the gone sentinel, still armed, no fetch",
			status, lastErr, armed, rig.fetcher.calls)
	}
}
