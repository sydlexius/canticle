package prune

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

// markGone records path as first seen gone the given time ago; a mark past the
// grace period is also confirmed, long enough ago to be deleted by the next sweep.
func markGone(t *testing.T, ctx context.Context, sqlDB *sql.DB, path string, ago time.Duration) {
	t.Helper()
	var confirmed any
	if ago >= goneGrace {
		confirmed = time.Now().Add(-2 * goneConfirmGap).Unix()
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO prune_gone_since (path, first_seen, confirmed_at) VALUES (?, ?, ?)`,
		path, time.Now().Add(-ago).Unix(), confirmed); err != nil {
		t.Fatal(err)
	}
}

func firstSeen(t *testing.T, ctx context.Context, sqlDB *sql.DB, path string) (at int64) {
	t.Helper()
	if err := sqlDB.QueryRowContext(ctx, `SELECT first_seen FROM prune_gone_since WHERE path = ?`, path).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func marked(t *testing.T, ctx context.Context, sqlDB *sql.DB) (n int) {
	t.Helper()
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM prune_gone_since`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// THE #1262 AGE-OUT TEST, lifecycle and cost together. A row gone inside a
// surviving directory is retained, costs nothing more while it waits (one stat
// per directory, across restarts: a fresh Pruner each sweep). The first sweep
// after the grace period only confirms; a sweep at least goneConfirmGap later
// deletes the row with its backup record and its mark. A due mark costs two
// stats of that one file and one read of its directory. A dry run plans the
// same delete, and the row's own sidecar is not a replacement.
func TestSweepDirectory_GoneRowAgesOutAfterGrace(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirs := seedAlbums(t, ctx, sqlDB, libID, root, 4)
	sweepDir(t, ctx, New(sqlDB))
	victim := filepath.Join(dirs[0], "0-00.mp3")
	err := os.Remove(victim)
	for _, ext := range []string{".lrc", ".ELRC", ".txt"} {
		err = errors.Join(err, os.WriteFile(filepath.Join(dirs[0], "0-00"+ext), []byte("words"), 0o600))
	}
	if err != nil {
		t.Fatal(err)
	}
	age(t, dirs[0])

	sqlArrange(`UPDATE work_queue SET outcome_type = 'synced', sync_tier = 'line', timing_outcome = 'ok', lyric_edited_at = 'edited'`)(t, ctx, sqlDB, 0, "")
	day := 24 * time.Hour
	start := time.Now()
	var reported []PrunedRow
	for _, step := range []struct {
		after  time.Duration
		dryRun bool
		stats  int // 4 directories, plus what the step examines
		pruned int
		reads  int // directory reads
	}{
		{0, false, 7, 0, 0},       // the changed album's three row files, once
		{6 * day, false, 4, 0, 0}, // waiting costs nothing
		{goneGrace - time.Minute, false, 4, 0, 0},
		{goneGrace, true, 6, 0, 1},  // the due file, its re-stat, its directory
		{goneGrace, false, 6, 0, 1}, // confirmed, nothing deleted
		{goneGrace + goneConfirmGap - time.Minute, false, 6, 0, 1},
		{goneGrace + goneConfirmGap, true, 6, 1, 1},
		{goneGrace + goneConfirmGap, false, 6, 1, 1}, // the apply matches the plan
		{goneGrace + day, false, 4, 0, 0},
	} {
		p, calls := countingPruner(sqlDB)
		p.now = func() time.Time { return start.Add(step.after) }
		reads, readDir := 0, p.readDir
		p.readDir = func(dir string) ([]os.DirEntry, error) { reads++; return readDir(dir) }
		res, err := p.Sweep(ctx, SweepOptions{Granularity: Directory, DryRun: step.dryRun, Report: func(r PrunedRow) error {
			if !step.dryRun {
				reported = append(reported, r)
			}
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if extra := outside(*calls, dirs); len(extra) != 0 {
			t.Fatalf("+%v statted outside the album: %v", step.after, extra)
		}
		if len(*calls) != step.stats || reads != step.reads {
			t.Errorf("+%v dry=%v: %d stats %d reads, want %d/%d: %v", step.after, step.dryRun, len(*calls), reads, step.stats, step.reads, *calls)
		}
		if len(res.Pruned) != step.pruned || res.AgedOut != step.pruned || res.WorkItems != step.pruned || res.ScanResults != step.pruned {
			t.Fatalf("+%v dry=%v: pruned=%d aged=%d work=%d scan=%d, want %d each", step.after, step.dryRun,
				len(res.Pruned), res.AgedOut, res.WorkItems, res.ScanResults, step.pruned)
		}
		wantRows, deleted := 12, step.after >= goneGrace+goneConfirmGap && !step.dryRun
		if deleted {
			wantRows = 11
		}
		if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != wantRows || wq != wantRows {
			t.Fatalf("+%v dry=%v: scan=%d wq=%d, want %d each", step.after, step.dryRun, sr, wq, wantRows)
		}
		aging, due, _, err := p.AgingCounts(ctx)
		wantAging, wantDue := 1, 0
		if step.after >= goneGrace {
			wantAging, wantDue = 0, 1
			if deleted {
				wantDue = 0
			}
		}
		if deleted && marked(t, ctx, sqlDB) != 0 {
			t.Errorf("+%v: the deleted row's mark is still stored", step.after)
		}
		if err != nil || aging != wantAging || due != wantDue {
			t.Errorf("+%v dry=%v: aging=%d due=%d err=%v, want %d/%d", step.after, step.dryRun, aging, due, err, wantAging, wantDue)
		}
	}
	if len(reported) != 1 || reported[0].SourcePath != victim || !reported[0].AgedOut || reported[0].Corrects ||
		len(reported[0].Inputs) != 1 || reported[0].Inputs[0].SourcePath != victim || len(reported[0].States) != 1 || reported[0].States[0] != (WorkState{"done", "synced", "line", "ok", "edited"}) {
		t.Fatalf("backup records = %+v, want one restorable aged-out record for %q", reported, victim)
	}
}

// arranger runs after the row's file is removed and marked; it returns the
// stat the sweep uses (nil: os.Stat).
type arranger = func(t *testing.T, ctx context.Context, sqlDB *sql.DB, libID int64, track string) func(string) (fs.FileInfo, error)

func sqlArrange(query string, args ...any) arranger {
	return func(t *testing.T, ctx context.Context, sqlDB *sql.DB, _ int64, _ string) func(string) (fs.FileInfo, error) {
		if _, err := sqlDB.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
		return nil
	}
}

// present puts files beside the track (indexed with mbid when index is set).
func present(index bool, mbid string, names ...string) arranger {
	return func(t *testing.T, ctx context.Context, sqlDB *sql.DB, libID int64, track string) func(string) (fs.FileInfo, error) {
		for _, name := range names {
			if path := filepath.Join(filepath.Dir(track), name); index {
				seedPresentScanResult(t, ctx, sqlDB, libID, path, mbid, "")
			} else if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
}

// titled: no identity, and a renamed file has its title (`scan reconcile-paths` would relink it).
func titled(status string) arranger {
	return func(t *testing.T, ctx context.Context, sqlDB *sql.DB, libID int64, track string) func(string) (fs.FileInfo, error) {
		seedNamedPresent(t, ctx, sqlDB, libID, filepath.Join(filepath.Dir(track), "renamed.flac"), "Artist", filepath.Base(track))
		return sqlArrange(`UPDATE scan_results SET recording_mbid = ''; UPDATE work_queue SET status = ?`, status)(t, ctx, sqlDB, 0, "")
	}
}

// onSecondStat runs hook in place of the track's second stat (the delete's re-stat).
func onSecondStat(hook func(t *testing.T, ctx context.Context, sqlDB *sql.DB) error) arranger {
	return func(t *testing.T, ctx context.Context, sqlDB *sql.DB, _ int64, track string) func(string) (fs.FileInfo, error) {
		seen := 0
		return func(path string) (fs.FileInfo, error) {
			if path == track {
				if seen++; seen == 2 {
					if err := hook(t, ctx, sqlDB); err != nil {
						return nil, err
					}
				}
			}
			return os.Stat(path)
		}
	}
}

// Nothing that leaves a doubt ages out, however old the mark.
func TestSweepDirectory_AgeOutRefusals(t *testing.T) {
	const old = goneGrace + time.Hour
	ahead := time.Now().Add(30 * 24 * time.Hour).Unix()
	for name, tc := range map[string]struct {
		processing bool
		unrecorded bool          // the report hook cannot write its record
		planned    int           // sources left in Result.Pruned (the plan) but not deleted
		ago        time.Duration // 0: not marked yet
		arrange    arranger
		wantMarked int
		relinked   int
		skipped    int // planned, then skipped by the delete's own guards
	}{
		"before the grace period, with a stale confirmation": {ago: goneGrace - time.Hour, wantMarked: 1, arrange: sqlArrange(`UPDATE prune_gone_since SET confirmed_at = 1`)},
		"in flight":                        {processing: true, ago: old, wantMarked: 1},
		"the caller cannot write a backup": {unrecorded: true, planned: 1, ago: old, wantMarked: 1},
		"not yet confirmed":                {ago: old, wantMarked: 1, arrange: sqlArrange(`UPDATE prune_gone_since SET confirmed_at = NULL`)},
		"confirmed too recently": {ago: old, wantMarked: 1,
			arrange: sqlArrange(`UPDATE prune_gone_since SET confirmed_at = ?`, time.Now().Add(time.Minute-goneConfirmGap).Unix())},
		"first seen by a clock that was ahead":                 {ago: old, wantMarked: 1, arrange: sqlArrange(`UPDATE prune_gone_since SET first_seen = ?`, ahead)},
		"confirmed by a clock that was ahead":                  {ago: old, wantMarked: 1, arrange: sqlArrange(`UPDATE prune_gone_since SET confirmed_at = ?`, ahead)},
		"its MBID is indexed and present under another name":   {ago: old, arrange: present(true, "mbid-nowhere", "renamed.flac")},
		"its MBID is present under another name: never marked": {arrange: present(true, "mbid-nowhere", "renamed.flac")},
		"an unindexed same-name file is on disk":               {ago: old, arrange: present(false, "", "01. A.flac")},
		"its MBID is indexed under no available root": {ago: old, arrange: sqlArrange(`INSERT INTO scan_results (library_id, file_path, artist, title, status, recording_mbid)
			SELECT id, '/unmounted/renamed.flac', 'Artist', 'Title', 'done', 'mbid-nowhere' FROM libraries`)},
		"one present file has its title, and the row is unavailable": {ago: old, arrange: titled("unavailable")},
		"one present file has its title, and the row is failed":      {ago: old, arrange: titled("failed")},
		"the row moved to another path after it was read": {ago: old, skipped: 1, arrange: onSecondStat(func(t *testing.T, ctx context.Context, sqlDB *sql.DB) error {
			sqlArrange(`UPDATE work_queue SET source_path = 'elsewhere.flac'`)(t, ctx, sqlDB, 0, "")
			sqlArrange(`UPDATE scan_results SET file_path = 'elsewhere.flac'`)(t, ctx, sqlDB, 0, "")
			return nil
		})},
		"file is back": {ago: old, arrange: func(t *testing.T, _ context.Context, _ *sql.DB, _ int64, track string) func(string) (fs.FileInfo, error) {
			return present(false, "", filepath.Base(track))(t, nil, nil, 0, track)
		}},
		"re-stat is not a clean not-exist": {ago: old, arrange: onSecondStat(func(*testing.T, context.Context, *sql.DB) error {
			return &fs.PathError{Op: "stat", Err: syscall.EIO}
		})},
		// The configured root is an empty mountpoint; the files are elsewhere.
		"library root unavailable": {ago: old, wantMarked: 1, arrange: func(t *testing.T, ctx context.Context, sqlDB *sql.DB, _ int64, _ string) func(string) (fs.FileInfo, error) {
			return sqlArrange(`UPDATE libraries SET path = ?`, t.TempDir())(t, ctx, sqlDB, 0, "")
		}},
		"directory is empty (an unmounted nested mountpoint)": {ago: old, arrange: func(t *testing.T, _ context.Context, _ *sql.DB, _ int64, track string) func(string) (fs.FileInfo, error) {
			if err := os.Remove(filepath.Join(filepath.Dir(track), "keep.txt")); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		"two present same-name files": {ago: old, arrange: present(true, "", "01. a.flac", "01. a.ogg")},
		"a replacement to relink to":  {ago: old, relinked: 1, arrange: present(true, "", "01. a.flac")},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			album := filepath.Join(root, "Artist", "Album")
			track := filepath.Join(album, "01. a.mp3")
			status := "done"
			if tc.processing {
				status = "processing"
			}
			seedRowWithIdentity(t, ctx, sqlDB, libID, track, "done", status, "mbid-nowhere", "")
			if err := errors.Join(os.WriteFile(filepath.Join(album, "keep.txt"), []byte("words"), 0o600), os.Remove(track)); err != nil {
				t.Fatal(err)
			}
			if tc.ago > 0 {
				markGone(t, ctx, sqlDB, track, tc.ago)
			}
			p := New(sqlDB)
			if tc.arrange != nil {
				if stat := tc.arrange(t, ctx, sqlDB, libID, track); stat != nil {
					p.stat = stat
				}
			}
			age(t, album)
			res, err := p.Sweep(ctx, SweepOptions{Granularity: Directory, Report: func(r PrunedRow) error {
				if tc.unrecorded {
					return ErrNotRecorded
				}
				if tc.skipped == 0 { // a planned delete is recorded first, then corrected
					t.Errorf("backup record written for %q", r.SourcePath)
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Pruned) != tc.skipped+tc.planned || res.PruneSkipped != tc.skipped || res.AgedOut != 0 || len(res.Relinked) != tc.relinked {
				t.Errorf("pruned=%d skipped=%d aged=%d relinked=%d, want %d/%d/0/%d", len(res.Pruned), res.PruneSkipped, res.AgedOut, len(res.Relinked), tc.skipped+tc.planned, tc.skipped, tc.relinked)
			}
			if sr, wq, _ := rowCounts(t, ctx, sqlDB); wq != 1 || sr < 1 {
				t.Errorf("scan_results=%d work_queue=%d, want the row kept", sr, wq)
			}
			var late int
			if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM prune_gone_since WHERE confirmed_at > ?1 OR (confirmed_at IS NOT NULL AND first_seen > ?1 - ?2)`,
				time.Now().Add(time.Minute).Unix(), int64(goneGrace/time.Second)).Scan(&late); err != nil || late != 0 {
				t.Errorf("confirmations later than now, or on a mark not due = %d (err %v), want them cleared", late, err)
			}
			if n := marked(t, ctx, sqlDB); n != tc.wantMarked {
				t.Errorf("grace marks = %d, want %d", n, tc.wantMarked)
			}
		})
	}
}

// A file that came back drops its mark, and one that goes again is marked
// afresh: the old mark is not inherited, so the sweep after its return retains
// rather than deletes. A mark no row names is dropped too.
func TestSweepDirectory_ReappearanceDropsTheMark(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	album := filepath.Join(root, "Artist", "Album")
	track := filepath.Join(album, "01. a.mp3")
	seedRow(t, ctx, sqlDB, libID, track, "done", "done")
	seedRow(t, ctx, sqlDB, libID, filepath.Join(album, "02. b.mp3"), "done", "done")
	markGone(t, ctx, sqlDB, track, goneGrace+time.Hour)
	markGone(t, ctx, sqlDB, filepath.Join(album, "no row names this.mp3"), time.Hour)
	age(t, album)
	if res := sweepDir(t, ctx, New(sqlDB)); len(res.Pruned) != 0 || marked(t, ctx, sqlDB) != 0 {
		t.Fatalf("present file: pruned=%d marks=%d, want 0/0", len(res.Pruned), marked(t, ctx, sqlDB))
	}
	if err := os.Remove(track); err != nil {
		t.Fatal(err)
	}
	age(t, album)
	later := New(sqlDB)
	later.now = func() time.Time { return time.Now().Add(time.Hour) }
	if res := sweepDir(t, ctx, later); len(res.Pruned) != 0 || marked(t, ctx, sqlDB) != 1 {
		t.Fatalf("gone again: pruned=%d marks=%d, want 0/1 (a fresh mark)", len(res.Pruned), marked(t, ctx, sqlDB))
	}
	if _, wq, _ := rowCounts(t, ctx, sqlDB); wq != 2 {
		t.Fatalf("work_queue rows = %d, want 2", wq)
	}
}

// One sweep under a clock that is behind (a boot before time sync) never shortens the week.
func TestSweepDirectory_ClockBehindNeverShortensTheWeek(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	track := filepath.Join(root, "Artist", "Album", "01. a.mp3")
	seedRow(t, ctx, sqlDB, libID, track, "done", "done")
	seedRow(t, ctx, sqlDB, libID, filepath.Join(filepath.Dir(track), "02. b.mp3"), "done", "done")
	if err := os.Remove(track); err != nil {
		t.Fatal(err)
	}
	markGone(t, ctx, sqlDB, track, 24*time.Hour)
	age(t, filepath.Dir(track))
	for _, at := range []time.Duration{-6 * 365 * 24 * time.Hour, 6 * time.Hour, 12 * time.Hour} {
		p := New(sqlDB)
		p.now = func() time.Time { return time.Now().Add(at) }
		sweepDir(t, ctx, p)
		if _, wq, _ := rowCounts(t, ctx, sqlDB); wq != 2 {
			t.Fatalf("clock %+v: work_queue rows = %d, want 2: the file has been gone under a week", at, wq)
		}
	}
}

// A mark is not STARTED under a clock that is behind: a file removed moments
// ago, with no mark yet, gets none from a sweep whose clock reads years early
// (the directory's mtime is later than that clock), and a fresh one, at the
// true time, from the next sweep.
func TestSweepDirectory_ClockBehindStartsNoMark(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	track := filepath.Join(root, "Artist", "Album", "01. a.mp3")
	seedRow(t, ctx, sqlDB, libID, track, "done", "done")
	seedRow(t, ctx, sqlDB, libID, filepath.Join(filepath.Dir(track), "02. b.mp3"), "done", "done")
	if err := os.Remove(track); err != nil {
		t.Fatal(err)
	}
	age(t, filepath.Dir(track))
	behind := New(sqlDB)
	behind.now = func() time.Time { return time.Now().Add(-6 * 365 * 24 * time.Hour) }
	sweepDir(t, ctx, behind)
	if n := marked(t, ctx, sqlDB); n != 0 {
		t.Fatalf("clock behind: marks = %d (first_seen %d), want none started", n, firstSeen(t, ctx, sqlDB, track))
	}
	p, at := New(sqlDB), time.Now()
	p.now = func() time.Time { return at }
	sweepDir(t, ctx, p)
	if n := marked(t, ctx, sqlDB); n != 1 || firstSeen(t, ctx, sqlDB, track) != at.Unix() {
		t.Fatalf("true time: marks = %d, want one fresh mark at %d", n, at.Unix())
	}
	later := New(sqlDB) // the week runs from the fresh mark: nothing is due two hours on
	later.now = func() time.Time { return at.Add(2 * time.Hour) }
	if res := sweepDir(t, ctx, later); res.AgedOut != 0 || len(res.Pruned) != 0 {
		t.Fatalf("two hours on: aged=%d pruned=%d, want none", res.AgedOut, len(res.Pruned))
	}
	if _, wq, _ := rowCounts(t, ctx, sqlDB); wq != 2 {
		t.Fatalf("two hours on: work_queue rows = %d, want 2: the file has been gone two hours", wq)
	}
}

// THE CIRCUIT BREAKER, and the report after it. More due rows than
// ageOutMaxPerSweep in one sweep deletes none and holds every mark, which then costs nothing and is
// deleted by no sweep, at the cap or under it. Rows confirmed in the ordinary way, at the cap, are each
// recorded first and deleted only if that worked; a row partly deleted is corrected.
func TestSweepDirectory_AgeOutCircuitBreaker(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	album := filepath.Join(root, "Artist", "Album")
	var tracks []string
	for i := 0; i <= ageOutMaxPerSweep; i++ {
		tracks = append(tracks, filepath.Join(album, fmt.Sprintf("%02d.mp3", i)))
		seedRowWithIdentity(t, ctx, sqlDB, libID, tracks[i], "done", "done", fmt.Sprintf("mbid-%d", i), "")
		if err := os.Remove(tracks[i]); err != nil {
			t.Fatal(err)
		}
		markGone(t, ctx, sqlDB, tracks[i], goneGrace+time.Hour)
	}
	if err := os.WriteFile(filepath.Join(album, "keep.txt"), []byte("words"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, album)
	for i, wantStats := range []int{1 + 2*len(tracks), 1, 1} { // tripping, then held
		p, calls := countingPruner(sqlDB)
		reads, readDir := 0, p.readDir
		p.readDir = func(dir string) ([]os.DirEntry, error) { reads++; return readDir(dir) }
		res := sweepDir(t, ctx, p)
		if _, wq, _ := rowCounts(t, ctx, sqlDB); res.AgeOutHeld != len(tracks) || res.AgedOut != 0 || wq != len(tracks) || marked(t, ctx, sqlDB) != len(tracks) || len(*calls) != wantStats || reads != wantStats/100 {
			t.Fatalf("over the cap, sweep %d: held=%d aged=%d rows=%d marks=%d, want %d/0/%d/%d; %d stats %d directory reads, want %d/%d", i, res.AgeOutHeld, res.AgedOut, wq, marked(t, ctx, sqlDB), len(tracks), len(tracks), len(tracks), len(*calls), reads, wantStats, wantStats/100)
		}
		if aging, due, held, err := p.AgingCounts(ctx); err != nil || aging != 0 || due != 0 || held != len(tracks) {
			t.Fatalf("over the cap, sweep %d: aging=%d due=%d held=%d err=%v, want every mark counted as held, none as due", i, aging, due, held, err)
		}
	}
	res, err := New(sqlDB).PrunePath(ctx, album) // the reactive pass is not the attended exit
	if _, wq, _ := rowCounts(t, ctx, sqlDB); err != nil || len(res.Pruned) != 0 || wq != len(tracks) {
		t.Fatalf("reactive pass over held marks: err=%v pruned=%d rows=%d, want none deleted", err, len(res.Pruned), wq)
	}
	sqlArrange(`DELETE FROM work_queue WHERE source_path = ?`, tracks[0])(t, ctx, sqlDB, 0, "") // one row is gone: the rest are at the cap
	sqlArrange(`DELETE FROM scan_results WHERE file_path = ?`, tracks[0])(t, ctx, sqlDB, 0, "")
	atCap, calls := countingPruner(sqlDB) // at the cap the held rows stay held, still for one stat
	res = sweepDir(t, ctx, atCap)
	if _, wq, _ := rowCounts(t, ctx, sqlDB); len(res.Pruned) != 0 || res.AgeOutHeld != ageOutMaxPerSweep || wq != ageOutMaxPerSweep || len(*calls) != 1 {
		t.Fatalf("held rows at the cap: pruned=%d held=%d rows=%d stats=%d, want 0/%d/%d/1: no sweep deletes a held row", len(res.Pruned), res.AgeOutHeld, wq, len(*calls), ageOutMaxPerSweep, ageOutMaxPerSweep)
	}
	// The rest of this test is about rows confirmed in the ordinary way, so the marks are made that.
	sqlArrange(`UPDATE prune_gone_since SET confirmed_at = ?`, time.Now().Add(-2*goneConfirmGap).Unix())(t, ctx, sqlDB, 0, "")
	reports := 0
	_, err = New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Directory, Report: func(PrunedRow) error { reports++; return errors.New("disk full") }})
	if _, wq, _ := rowCounts(t, ctx, sqlDB); err == nil || strings.Contains(err.Error(), root) || reports != ageOutMaxPerSweep || wq != ageOutMaxPerSweep ||
		!strings.Contains(err.Error(), "50 backup record(s) could not be written, first: disk full") {
		t.Fatalf("at the cap: err=%v reports=%d rows=%d, want a path-free count, %d reports, no row deleted", err, reports, wq, ageOutMaxPerSweep)
	}
	var recs []PrunedRow // the delete itself fails: every record written ahead is corrected to nothing
	sqlArrange(`CREATE TRIGGER fail BEFORE DELETE ON scan_results BEGIN SELECT RAISE(ABORT, 'no'); END`)(t, ctx, sqlDB, 0, "")
	_, err = New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Directory, Report: func(r PrunedRow) error { recs = append(recs, r); return nil }})
	if last := recs[len(recs)-1]; err == nil || len(recs) != 2*ageOutMaxPerSweep || !last.Corrects || len(last.WorkItemIDs)+len(last.ScanResultIDs) != 0 {
		t.Fatalf("failed delete: err=%v, %d records, the last %+v: want an error and a correction per record", err, len(recs), last)
	}
	sqlArrange(`DROP TRIGGER fail`)(t, ctx, sqlDB, 0, "")
	age(t, album) // examined whole, so stored again
	p := New(sqlDB)
	p.readDir = func(dir string) ([]os.DirEntry, error) { // after the last check, one queue row moves
		sqlArrange(`UPDATE work_queue SET source_path = 'elsewhere.flac' WHERE id = (SELECT max(id) FROM work_queue)`)(t, ctx, sqlDB, 0, "")
		return os.ReadDir(dir)
	}
	failing := &Pruner{readDir: func(dir string) ([]os.DirEntry, error) { e, _ := os.ReadDir(dir); return e, syscall.EIO }}
	if !failing.replacementOnDisk(tracks[1], map[string][]os.DirEntry{}) {
		t.Fatal("a directory read that returned entries and an error did not refuse")
	}
	res, err = p.Sweep(ctx, SweepOptions{Granularity: Directory, Report: func(r PrunedRow) error { recs = append(recs, r); return nil }})
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); err != nil || sr != 0 || wq != 1 || res.AgedOut != ageOutMaxPerSweep || res.PruneSkipped != 1 || res.WorkItems != ageOutMaxPerSweep-1 {
		t.Fatalf("one row partly moved: err=%v scan=%d wq=%d aged=%d skipped=%d work=%d, want 0/1/%d/1/%d", err, sr, wq, res.AgedOut, res.PruneSkipped, res.WorkItems, ageOutMaxPerSweep, ageOutMaxPerSweep-1)
	}
	if last := recs[len(recs)-1]; len(recs) != 3*ageOutMaxPerSweep+1 || !last.Corrects || len(last.WorkItemIDs) != 0 || len(last.ScanResultIDs) != 1 || last.SourcePath != tracks[len(tracks)-1] {
		t.Fatalf("%d backup records, the last %+v: want each row's again, then a correction listing only the scan row deleted", len(recs), last)
	}
	if sqlArrange(`DELETE FROM prune_dir_state WHERE gone_scan_id IS NULL`)(t, ctx, sqlDB, 0, ""); storedDirs(t, ctx, sqlDB) != 0 {
		t.Fatal("the directory was stored as still holding a gone row, though its gone rows were deleted")
	}
}

// A HELD ROW IS DELETED ONLY BY THE ATTENDED RUN. The breaker trips on one row over the cap; then files
// come back, so fewer than the cap are held: no periodic sweep deletes a held row or releases its mark,
// and one in an unchanged directory costs no stat and no directory read. Held rows count toward the cap
// for rows newly due: those are deleted while held plus due stays at or under it, and held too otherwise.
func TestSweepDirectory_HeldRowsAreDeletedOnlyByTheAttendedRun(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	album := filepath.Join(root, "Artist", "Album")
	gone := func(path string) { // a row whose file is gone, its mark due and confirmed
		t.Helper()
		seedRowWithIdentity(t, ctx, sqlDB, libID, path, "done", "done", "mbid-"+path, "")
		if err := errors.Join(os.Remove(path), os.WriteFile(filepath.Join(filepath.Dir(path), "keep.txt"), []byte("words"), 0o600)); err != nil {
			t.Fatal(err)
		}
		markGone(t, ctx, sqlDB, path, goneGrace+time.Hour)
		age(t, filepath.Dir(path))
	}
	var tracks []string
	for i := 0; i <= ageOutMaxPerSweep; i++ {
		tracks = append(tracks, filepath.Join(album, fmt.Sprintf("%02d.mp3", i)))
		gone(tracks[i])
	}
	heldMarks := func() (n int) {
		t.Helper()
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM prune_gone_since WHERE confirmed_at = ?`, heldMark).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// sweep runs one periodic sweep and checks what it deleted, what is held after it, and its cost.
	sweep := func(step string, now time.Time, wantAged, wantHeld, wantRows, wantStats, wantReads int) {
		t.Helper()
		p, calls := countingPruner(sqlDB)
		p.now = func() time.Time { return now }
		reads, readDir := 0, p.readDir
		p.readDir = func(dir string) ([]os.DirEntry, error) { reads++; return readDir(dir) }
		res := sweepDir(t, ctx, p)
		if _, wq, _ := rowCounts(t, ctx, sqlDB); res.AgedOut != wantAged || len(res.Pruned) != wantAged || res.AgeOutHeld != wantHeld || heldMarks() != wantHeld || wq != wantRows {
			t.Fatalf("%s: aged=%d pruned=%d held=%d held marks=%d rows=%d, want %d/%d/%d/%d/%d", step, res.AgedOut, len(res.Pruned), res.AgeOutHeld, heldMarks(), wq, wantAged, wantAged, wantHeld, wantHeld, wantRows)
		}
		if len(*calls) != wantStats || reads != wantReads {
			t.Fatalf("%s: %d stats %d directory reads, want %d/%d", step, len(*calls), reads, wantStats, wantReads)
		}
	}
	n := len(tracks) // 51
	sweep("the breaker trips", time.Now(), 0, n, n, 1+2*n, 1)

	// One file comes back: 50 are still gone, which is at the cap. The directory changed, so it is
	// examined (each row's stat, the re-stat of the 50, one read), and nothing is deleted.
	if err := os.WriteFile(tracks[0], []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, album)
	sweep("one file back, the changed directory", time.Now(), 0, n-1, n, 1+n+(n-1), 1)
	for i := 0; i < 2; i++ { // unchanged: a held mark costs no stat and no read, and is not deleted
		sweep("one file back, later sweeps", time.Now().Add(time.Duration(i+2)*goneConfirmGap), 0, n-1, n, 1, 0)
	}
	// A clock that stepped back does not turn a held mark into an ordinary one.
	sweep("the clock behind", time.Now().Add(-30*24*time.Hour), 0, n-1, n, 1, 0)

	// A second file comes back: 49 held. One row newly due elsewhere keeps the total at the cap and is
	// deleted; the held rows are not. Two more would take it over, so they are held as well.
	if err := os.WriteFile(tracks[1], []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, album)
	sweep("two files back", time.Now(), 0, n-2, n, 1+n+(n-2), 1)
	gone(filepath.Join(root, "Artist", "Other", "x.mp3"))
	// (Seeding a row is a scan insert, so the album, which holds gone rows, is examined again.)
	sweep("one newly due row, at the cap", time.Now(), 1, n-2, n, 2+n+(n-2)+2, 2)
	gone(filepath.Join(root, "Artist", "Third", "y.mp3"))
	gone(filepath.Join(root, "Artist", "Third", "z.mp3"))
	sweep("two newly due rows, over the cap", time.Now(), 0, n, n+2, 2+n+(n-2)+4, 2)
	sweep("all held", time.Now(), 0, n, n+2, 2, 0)

	// The attended run plans them all and deletes them all, and no mark is left held.
	dry, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: true})
	if err != nil || len(dry.Pruned) != n || dry.AgedOut != n {
		t.Fatalf("attended dry run: err=%v pruned=%d aged=%d, want the %d held rows planned", err, len(dry.Pruned), dry.AgedOut, n)
	}
	res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact})
	if _, wq, _ := rowCounts(t, ctx, sqlDB); err != nil || res.AgedOut != n || wq != 2 || heldMarks() != 0 {
		t.Fatalf("attended apply: err=%v aged=%d rows=%d held marks=%d, want the %d held rows deleted, the 2 present rows left and no mark held", err, res.AgedOut, wq, heldMarks(), n)
	}
}

// A QUEUE ROW TWO LIBRARIES SHARE IS NOT AGED OUT. work_queue is unique per song, so two libraries'
// copies link one queue row; when one copy is gone and its mark is due and confirmed, no run (scoped
// attended, unscoped attended, periodic) deletes the row the other library's present file still uses.
func TestSweep_AgeOutLeavesARowAnotherLibraryLinks(t *testing.T) {
	for name, opts := range map[string]SweepOptions{
		"attended, scoped to the library": {Granularity: Exact},
		"attended, unscoped":              {Granularity: Exact},
		"periodic":                        {Granularity: Directory},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, sqlDB, libA, rootA := openSeeded(t)
			rootB := filepath.Join(filepath.Dir(rootA), "other")
			libB, err := library.New(sqlDB).Add(ctx, rootB, "other", models.LibrarySettings{})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(name, "scoped to") {
				opts.LibraryID = &libA
			}
			goneA := filepath.Join(rootA, "Artist", "Album", "song.mp3")
			seedRow(t, ctx, sqlDB, libA, goneA, "done", "done")
			srB := seedPresentScanResult(t, ctx, sqlDB, libB.ID, filepath.Join(rootB, "Artist", "Album", "song.flac"), "", "")
			sqlArrange(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) SELECT id, ? FROM work_queue`, srB)(t, ctx, sqlDB, 0, "")
			if err := errors.Join(os.Remove(goneA), os.WriteFile(filepath.Join(filepath.Dir(goneA), "keep.txt"), []byte("words"), 0o600)); err != nil {
				t.Fatal(err)
			}
			age(t, filepath.Dir(goneA))
			markGone(t, ctx, sqlDB, goneA, goneGrace+time.Hour)
			res, err := New(sqlDB).Sweep(ctx, opts)
			if sr, wq, links := rowCounts(t, ctx, sqlDB); err != nil || len(res.Pruned) != 0 || res.AgedOut != 0 || sr != 2 || wq != 1 || links != 2 || !scanResultExists(t, ctx, sqlDB, srB) {
				t.Fatalf("err=%v pruned=%d aged=%d scan=%d wq=%d links=%d, want nothing deleted: the other library's file still links the queue row", err, len(res.Pruned), res.AgedOut, sr, wq, links)
			}
			// Control: with the other library's link gone, the same row ages out.
			sqlArrange(`DELETE FROM scan_results WHERE id = ?`, srB)(t, ctx, sqlDB, 0, "")
			sqlArrange(`DELETE FROM prune_gone_since`)(t, ctx, sqlDB, 0, "")
			markGone(t, ctx, sqlDB, goneA, goneGrace+time.Hour)
			sqlArrange(`DELETE FROM prune_dir_state`)(t, ctx, sqlDB, 0, "")
			res, err = New(sqlDB).Sweep(ctx, opts)
			if sr, wq, _ := rowCounts(t, ctx, sqlDB); err != nil || res.AgedOut != 1 || sr != 0 || wq != 0 {
				t.Fatalf("control, unshared: err=%v aged=%d scan=%d wq=%d, want the row aged out", err, res.AgedOut, sr, wq)
			}
		})
	}
}

// Two libraries that both index the SAME gone file: a run scoped to one sees only its own row, so it
// leaves the shared queue row; the unscoped run gathered both rows and ages the source out whole.
func TestSweep_AgeOutOfAFileTwoLibrariesIndex(t *testing.T) {
	ctx, sqlDB, libA, root := openSeeded(t)
	libB, err := library.New(sqlDB).Add(ctx, filepath.Join(root, "Artist"), "nested", models.LibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "Artist", "Album", "song.mp3")
	seedRow(t, ctx, sqlDB, libA, gone, "done", "done")
	sqlArrange(`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename) VALUES (?, ?, 'Artist', 'Title', 'done', '', '')`, libB.ID, gone)(t, ctx, sqlDB, 0, "")
	sqlArrange(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) SELECT (SELECT id FROM work_queue), max(id) FROM scan_results`)(t, ctx, sqlDB, 0, "")
	if err := errors.Join(os.Remove(gone), os.WriteFile(filepath.Join(filepath.Dir(gone), "keep.txt"), []byte("words"), 0o600)); err != nil {
		t.Fatal(err)
	}
	markGone(t, ctx, sqlDB, gone, goneGrace+time.Hour)
	res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact, LibraryID: &libA})
	if sr, wq, links := rowCounts(t, ctx, sqlDB); err != nil || res.AgedOut != 0 || sr != 2 || wq != 1 || links != 2 {
		t.Fatalf("scoped: err=%v aged=%d scan=%d wq=%d links=%d, want nothing deleted: the other library's row links the queue row", err, res.AgedOut, sr, wq, links)
	}
	res, err = New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact})
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); err != nil || res.AgedOut != 1 || sr != 0 || wq != 0 {
		t.Fatalf("unscoped: err=%v aged=%d scan=%d wq=%d, want the source aged out with both libraries' rows", err, res.AgedOut, sr, wq)
	}
}
