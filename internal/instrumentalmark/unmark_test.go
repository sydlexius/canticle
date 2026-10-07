package instrumentalmark

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

func (f *fixture) marker() string { return filepath.Join(f.dir, "song.txt") }

func (f *fixture) protected(t *testing.T) bool {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM work_queue WHERE id = ? AND `+queue.HandProtectedPredicate, f.id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func (f *fixture) markFirst(t *testing.T) {
	t.Helper()
	if _, err := f.m.Mark(f.ctx, f.id, Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestUnmarkRemovesMarkerBacksItUpAndRequeues(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	want, err := os.ReadFile(f.marker())
	if err != nil {
		t.Fatal(err)
	}
	if !f.protected(t) {
		t.Fatal("a marked row must be hand-protected")
	}
	rec := &recorder{}
	report := func(r Record) error {
		if !f.exists("song.txt") || !f.marked(t) {
			t.Errorf("Report ran after a change: marker present=%v marked=%v", f.exists("song.txt"), f.marked(t))
		}
		return rec.report(r)
	}
	res, err := f.m.Unmark(f.ctx, f.id, Options{Report: report})
	if err != nil || res.Outcome != OutcomeUnmarked || res.FilesBackedUp != 1 {
		t.Fatalf("Unmark = %+v, %v; want unmarked with 1 file", res, err)
	}
	if len(rec.recs) != 1 || rec.recs[0].Op != OpUnmark || rec.recs[0].WorkItemID != f.id ||
		filepath.Base(rec.recs[0].Path) != "song.txt" || !bytes.Equal(rec.recs[0].Content, want) {
		t.Fatalf("records = %+v; want one restorable unmark record of the marker bytes", rec.recs)
	}
	if f.exists("song.txt") || f.marked(t) || f.protected(t) {
		t.Errorf("marker present=%v marked=%v protected=%v; want all cleared", f.exists("song.txt"), f.marked(t), f.protected(t))
	}
	item, err := f.q.Dequeue(f.ctx)
	if err != nil || item.ID != f.id {
		t.Fatalf("Dequeue = %+v, %v; want the unmarked row back in the queue", item, err)
	}
}

func TestUnmarkOfAnUnmarkedRowChangesNothing(t *testing.T) {
	f := newFixture(t)
	// A manual marker on disk without a mark on the row is not Unmark's to touch.
	f.markFirst(t)
	body, err := os.ReadFile(f.marker())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Unmark(f.ctx, f.id, Options{Report: (&recorder{}).report}); err != nil {
		t.Fatal(err)
	}
	f.write(t, "song.txt", string(body))
	if !lyrics.ManualMarkerOnDisk(f.marker()) {
		t.Fatal("setup: want a manual marker on disk")
	}
	rec := &recorder{}
	res, err := f.m.Unmark(f.ctx, f.id, Options{Report: rec.report})
	if err != nil || res.Outcome != OutcomeNotMarked || len(rec.recs) != 0 || !f.exists("song.txt") || f.marked(t) {
		t.Errorf("Unmark = %+v, %v, %d records, marker present=%v; want a no-op", res, err, len(rec.recs), f.exists("song.txt"))
	}
}

func TestUnmarkDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	res, err := f.m.Unmark(f.ctx, f.id, Options{DryRun: true, Report: func(Record) error {
		t.Error("dry run called Report")
		return nil
	}})
	if err != nil || res.Outcome != OutcomeDryRun || res.FilesBackedUp != 1 {
		t.Fatalf("Unmark = %+v, %v; want dry_run with 1 file", res, err)
	}
	if !f.exists("song.txt") || !f.marked(t) {
		t.Error("a dry run changed something")
	}
}

func TestUnmarkRefusesInFlightAndMissingRows(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	if _, err := f.db.ExecContext(f.ctx, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, f.id); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if _, err := f.m.Unmark(f.ctx, f.id, Options{Report: rec.report}); !errors.Is(err, queue.ErrManualInstrumentalInFlight) {
		t.Fatalf("err = %v; want ErrManualInstrumentalInFlight", err)
	}
	if len(rec.recs) != 0 || !f.exists("song.txt") || !f.marked(t) {
		t.Error("an in-flight refusal changed something")
	}
	if _, err := f.m.Unmark(f.ctx, 99999, Options{}); !errors.Is(err, queue.ErrManualInstrumentalNotFound) {
		t.Errorf("missing row err = %v", err)
	}
}

func TestUnmarkRequiresABackupSinkAndAbortsOnBackupFailure(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	if _, err := f.m.Unmark(f.ctx, f.id, Options{}); !errors.Is(err, ErrNoBackupSink) {
		t.Fatalf("err = %v; want ErrNoBackupSink", err)
	}
	if _, err := f.m.Unmark(f.ctx, f.id, Options{Report: func(Record) error { return errors.New("disk full") }}); err == nil {
		t.Fatal("want a backup error")
	}
	if !f.exists("song.txt") || !f.marked(t) {
		t.Error("a failed backup left a change behind")
	}
}

func TestUnmarkLeavesATxtThatIsNoLongerAManualMarkerButBacksItUp(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	f.write(t, "song.txt", "real lyrics now\n")
	rec := &recorder{}
	res, err := f.m.Unmark(f.ctx, f.id, Options{Report: rec.report})
	if err != nil || res.Outcome != OutcomeUnmarked || res.FilesBackedUp != 1 {
		t.Fatalf("Unmark = %+v, %v; want unmarked with 1 file backed up", res, err)
	}
	if len(rec.recs) != 1 || rec.recs[0].Op != OpUnmark || string(rec.recs[0].Content) != "real lyrics now\n" {
		t.Fatalf("records = %+v; want the real lyrics backed up before the re-queue", rec.recs)
	}
	if b, _ := os.ReadFile(f.marker()); string(b) != "real lyrics now\n" || f.marked(t) {
		t.Errorf("txt = %q marked=%v; want the real lyrics kept and the row unmarked", b, f.marked(t))
	}
}

func TestUnmarkBacksUpALyricFileBesideTheMarker(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	f.write(t, "song.lrc", "[00:01.00]hi\n")
	rec := &recorder{}
	res, err := f.m.Unmark(f.ctx, f.id, Options{Report: rec.report})
	if err != nil || res.FilesBackedUp != 2 || len(rec.recs) != 2 || !f.exists("song.lrc") || f.exists("song.txt") {
		t.Errorf("Unmark = %+v, %v, %d records, lrc kept=%v; want 2 records, lrc kept, marker gone", res, err, len(rec.recs), f.exists("song.lrc"))
	}
}

func TestUnmarkInvalidatesACacheEntryStoredAfterTheMark(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	c := cache.New(f.db)
	if err := c.Store(f.ctx, "Artist", "Title", 0, "stored after the mark"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Unmark(f.ctx, f.id, Options{Report: (&recorder{}).report}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Lookup(f.ctx, "Artist", "Title", 0); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("cache after unmark = %q, %v; want no entry (a stale one would satisfy the re-queued row)", got, err)
	}
}

func TestUnmarkRechecksImmediatelyBeforeRemoval(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	// Real lyrics land after the listing and the backup, before the unlink.
	report := func(Record) error {
		f.write(t, "song.txt", "real lyrics landed late\n")
		return nil
	}
	res, err := f.m.Unmark(f.ctx, f.id, Options{Report: report})
	if err != nil || res.Outcome != OutcomeUnmarked {
		t.Fatalf("Unmark = %+v, %v", res, err)
	}
	if b, _ := os.ReadFile(f.marker()); string(b) != "real lyrics landed late\n" {
		t.Errorf("txt = %q; want the late real lyrics left alone", b)
	}
}

func TestUnmarkRowFailureKeepsTheRowMarkedAndRetryRecovers(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	if _, err := f.db.Exec(`CREATE TRIGGER block_unmark BEFORE UPDATE OF manual_instrumental_at ON work_queue BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := f.m.Unmark(f.ctx, f.id, Options{Report: (&recorder{}).report})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, leak := range []string{f.dir, f.root, "song"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
	// Partial state: the marker is gone but the row is still marked, so it is
	// protected and nothing is re-queued beside a marker.
	if f.exists("song.txt") || !f.marked(t) || !f.protected(t) {
		t.Fatalf("partial state: marker present=%v marked=%v protected=%v; want marker gone, row still marked", f.exists("song.txt"), f.marked(t), f.protected(t))
	}
	if _, derr := f.db.Exec(`DROP TRIGGER block_unmark`); derr != nil {
		t.Fatal(derr)
	}
	// A second Unmark has no marker left to back up and finishes the job.
	res, err := f.m.Unmark(f.ctx, f.id, Options{})
	if err != nil || res.Outcome != OutcomeUnmarked || f.marked(t) || f.protected(t) {
		t.Errorf("retry = %+v, %v marked=%v protected=%v; want the row unmarked", res, err, f.marked(t), f.protected(t))
	}
}

func TestUnmarkRowFailureIsAlsoRepairedByMark(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	if _, err := f.db.Exec(`CREATE TRIGGER block_unmark BEFORE UPDATE OF manual_instrumental_at ON work_queue BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Unmark(f.ctx, f.id, Options{Report: (&recorder{}).report}); err == nil {
		t.Fatal("want an error")
	}
	res, err := f.m.Mark(f.ctx, f.id, Options{})
	if err != nil || res.Outcome != OutcomeMarked || !f.marked(t) || !lyrics.ManualMarkerOnDisk(f.marker()) {
		t.Errorf("Mark = %+v, %v; want marker rewritten beside the marked row", res, err)
	}
}

func TestUnmarkRemovalFailureKeepsRowMarkedAndRetryFinishes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newFixture(t)
	f.markFirst(t)
	if err := os.Chmod(f.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.dir, 0o755) })
	_, err := f.m.Unmark(f.ctx, f.id, Options{Report: (&recorder{}).report})
	if err == nil {
		t.Fatal("want a removal error")
	}
	if strings.Contains(err.Error(), f.dir) || strings.Contains(err.Error(), "song") {
		t.Errorf("error leaks a path: %q", err)
	}
	if !f.exists("song.txt") || !f.marked(t) {
		t.Fatal("a failed removal must leave marker and mark together")
	}
	if err := os.Chmod(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if res, err := f.m.Unmark(f.ctx, f.id, Options{Report: (&recorder{}).report}); err != nil || res.Outcome != OutcomeUnmarked || f.exists("song.txt") {
		t.Errorf("retry = %+v, %v", res, err)
	}
}

func TestUnmarkRefusesASymlinkedLyricFileAndTouchesNothing(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	body, err := os.ReadFile(f.marker())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.txt")
	if err := os.WriteFile(target, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.marker()); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if _, err := f.m.Unmark(f.ctx, f.id, Options{Report: rec.report}); !errors.Is(err, ErrSymlinkedSidecar) {
		t.Fatalf("err = %v; want ErrSymlinkedSidecar (the shared inventory refuses a symlink, as Mark does)", err)
	}
	if !f.marked(t) {
		t.Error("a refused unmark must leave the row marked")
	}
	if fi, err := os.Lstat(f.marker()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was removed or replaced: %v", err)
	}
	if b, err := os.ReadFile(target); err != nil || !bytes.Equal(b, body) || len(rec.recs) != 0 {
		t.Errorf("the symlink target was touched or backed up (%d records): %v", len(rec.recs), err)
	}
}

// A Mark that arrives while an Unmark on the same row is mid-flight (here, in
// the middle of its backup) must wait for it, so the pair ends consistent.
func TestMarkWaitsForAnUnmarkOnTheSameRow(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	markDone := make(chan error, 1)
	report := func(Record) error {
		go func() { _, err := f.m.Mark(f.ctx, f.id, Options{}); markDone <- err }()
		select {
		case err := <-markDone:
			markDone <- err // put it back so the final receive cannot hang
			t.Errorf("Mark finished (err=%v) while an Unmark held the row", err)
		case <-time.After(150 * time.Millisecond):
		}
		return nil
	}
	if res, err := f.m.Unmark(f.ctx, f.id, Options{Report: report}); err != nil || res.Outcome != OutcomeUnmarked {
		t.Fatalf("Unmark = %+v, %v", res, err)
	}
	if err := <-markDone; err != nil {
		t.Fatal(err)
	}
	if got, want := lyrics.ManualMarkerOnDisk(f.marker()), f.marked(t); got != want || !got {
		t.Errorf("marker on disk = %v, row marked = %v; want both (the Mark ran last)", got, want)
	}
}

// waitBlocked polls until a goroutine is queued behind the held lock for id
// (rowLock.n counts the holder plus waiters); the deadline is only a failure bound.
func waitBlocked(t *testing.T, m *Marker, id int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m.locks.mu.Lock()
		l := m.locks.m[id]
		n := 0
		if l != nil {
			n = l.n
		}
		m.locks.mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the call never queued behind the row lock")
}

// Deterministic lock check: while the test holds the row's lock, Mark and
// Unmark must not reach their first observable step (Report); once released,
// they do.
func TestMarkAndUnmarkWaitForTheRowLock(t *testing.T) {
	for _, name := range []string{"Unmark", "Mark"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.markFirst(t)
			f.write(t, "song.lrc", "[00:01.00]hi\n") // gives Mark a file to report
			reached := make(chan struct{}, 8)
			opts := Options{Report: func(Record) error { reached <- struct{}{}; return nil }}
			release := f.m.locks.lock(f.id)
			done := make(chan error, 1)
			go func() {
				var err error
				if name == "Unmark" {
					_, err = f.m.Unmark(f.ctx, f.id, opts)
				} else {
					_, err = f.m.Mark(f.ctx, f.id, opts)
				}
				done <- err
			}()
			waitBlocked(t, f.m, f.id)
			select {
			case <-reached:
				t.Fatalf("%s reached Report while the row lock was held", name)
			default:
			}
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s never ran after the lock was released", name)
			}
			if len(reached) == 0 {
				t.Errorf("%s never reached Report after release", name)
			}
		})
	}
}

func TestUnmarkWithdrawnMidCallStillReportsTheFilesItHandled(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	rec := &recorder{}
	// Another process unmarks the row after this call loaded it and while it
	// is handing files to Report.
	report := func(r Record) error {
		if _, err := f.m.unmark(f.ctx, f.id, [][2]string{{"Artist", "Title"}}); err != nil {
			t.Fatal(err)
		}
		return rec.report(r)
	}
	res, err := f.m.Unmark(f.ctx, f.id, Options{Report: report})
	if err != nil || res.Outcome != OutcomeUnmarked || res.FilesBackedUp != 1 {
		t.Fatalf("Unmark = %+v, %v; want unmarked with 1 file (not not_marked: a file was reported and removed)", res, err)
	}
	if f.exists("song.txt") {
		t.Error("marker still present")
	}
}

// afterClear is the seam: it runs right after the clearing transaction commits,
// standing in for a Mark in another process that wrote a marker in that window.
func TestUnmarkSweepsAMarkerThatAppearedBeforeTheRowWasCleared(t *testing.T) {
	f := newFixture(t)
	f.markFirst(t)
	rec := &recorder{}
	f.m.afterClear = func() {
		song := models.Song{Track: models.Track{ArtistName: "Artist", TrackName: "Title", Instrumental: 1}, WinningLane: lyrics.ManualLaneName}
		if err := f.m.w.WriteManualMarker(song, "song.flac", f.dir); err != nil {
			t.Fatal(err)
		}
	}
	res, err := f.m.Unmark(f.ctx, f.id, Options{Report: rec.report})
	if err != nil || res.Outcome != OutcomeUnmarked {
		t.Fatalf("Unmark = %+v, %v", res, err)
	}
	if f.exists("song.txt") {
		t.Error("a marker that appeared after the removal loop survived the unmark")
	}
	if len(rec.recs) != 2 {
		t.Errorf("records = %d; want the original marker and the late one, each backed up before removal", len(rec.recs))
	}
}
