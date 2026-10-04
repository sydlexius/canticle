package prune

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// markGone records path as first seen gone the given time ago.
func markGone(t *testing.T, ctx context.Context, sqlDB *sql.DB, path string, ago time.Duration) {
	t.Helper()
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO prune_gone_since (path, first_seen) VALUES (?, ?)`,
		path, time.Now().Add(-ago).Unix()); err != nil {
		t.Fatal(err)
	}
}

func marked(t *testing.T, ctx context.Context, sqlDB *sql.DB) (n int) {
	t.Helper()
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM prune_gone_since`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func firstSeen(t *testing.T, ctx context.Context, sqlDB *sql.DB, path string) (at int64) {
	t.Helper()
	if err := sqlDB.QueryRowContext(ctx, `SELECT first_seen FROM prune_gone_since WHERE path = ?`, path).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// THE #1262 GONE-MARK TEST, lifecycle and cost together. A row gone inside a
// surviving directory is retained and marked once, and the mark costs nothing
// more while it stands (one stat per directory, across restarts: a fresh
// Pruner each sweep). NOTHING IS DELETED ON A MARK, however old: the row, its
// scan_results row and the mark are all still there a month on, and the mark
// still carries the time it was first written.
func TestSweepDirectory_GoneRowIsMarkedAndKept(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dirs := seedAlbums(t, ctx, sqlDB, libID, root, 4)
	sweepDir(t, ctx, New(sqlDB))
	victim := filepath.Join(dirs[0], "0-00.mp3")
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	age(t, dirs[0])

	day := 24 * time.Hour
	start := time.Now()
	for _, step := range []struct {
		after  time.Duration
		dryRun bool
		stats  int // 4 directories, plus what the step examines
		marks  int
		touch  bool // the album's mtime moves first, so its rows are examined again
	}{
		{0, true, 7, 0, false},        // a dry run stores no mark
		{0, false, 7, 1, false},       // the changed album's three row files, once
		{6 * day, false, 4, 1, false}, // a standing mark costs nothing
		{goneGrace - time.Minute, false, 4, 1, false},
		{goneGrace, false, 4, 1, false},
		{goneGrace + day, true, 4, 1, false},
		{30 * day, false, 4, 1, false},
		{31 * day, false, 7, 1, true}, // examined again a month on: still kept, same mark
	} {
		if step.touch {
			if err := os.Chtimes(dirs[0], start.Add(-2*time.Hour), start.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		p, calls := countingPruner(sqlDB)
		p.now = func() time.Time { return start.Add(step.after) }
		res, err := p.Sweep(ctx, SweepOptions{Granularity: Directory, DryRun: step.dryRun, Report: func(r PrunedRow) error {
			t.Errorf("+%v: backup record written for %q", step.after, r.SourcePath)
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if extra := outside(*calls, dirs); len(extra) != 0 {
			t.Fatalf("+%v statted outside the album: %v", step.after, extra)
		}
		if len(*calls) != step.stats {
			t.Errorf("+%v dry=%v: %d stats, want %d: %v", step.after, step.dryRun, len(*calls), step.stats, *calls)
		}
		if len(res.Pruned) != 0 || res.WorkItems != 0 || res.ScanResults != 0 {
			t.Fatalf("+%v dry=%v: pruned=%d work=%d scan=%d, want none", step.after, step.dryRun, len(res.Pruned), res.WorkItems, res.ScanResults)
		}
		if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 12 || wq != 12 {
			t.Fatalf("+%v dry=%v: scan=%d wq=%d, want all 12 kept", step.after, step.dryRun, sr, wq)
		}
		if n := marked(t, ctx, sqlDB); n != step.marks {
			t.Fatalf("+%v dry=%v: marks = %d, want %d", step.after, step.dryRun, n, step.marks)
		}
		if step.marks == 0 {
			continue
		}
		if at := firstSeen(t, ctx, sqlDB, victim); at != start.Unix() {
			t.Errorf("+%v: first_seen = %d, want it left at %d", step.after, at, start.Unix())
		}
		wantAging, wantDue := 1, 0
		if step.after >= goneGrace {
			wantAging, wantDue = 0, 1
		}
		if aging, due, err := p.AgingCounts(ctx); err != nil || aging != wantAging || due != wantDue {
			t.Errorf("+%v dry=%v: aging=%d due=%d err=%v, want %d/%d", step.after, step.dryRun, aging, due, err, wantAging, wantDue)
		}
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

// Which gone rows are marked. A row that may have a replacement is not, and
// loses a mark it had; the row itself is kept in every case.
func TestSweepDirectory_GoneMarkHolds(t *testing.T) {
	const old = goneGrace + time.Hour
	for name, tc := range map[string]struct {
		processing bool
		ago        time.Duration // 0: not marked yet
		arrange    arranger
		wantMarked int
		relinked   int
	}{
		"nothing to relink it to: marked":                      {wantMarked: 1},
		"nothing to relink it to: the mark stands":             {ago: old, wantMarked: 1},
		"in flight: not examined, the mark stands":             {processing: true, ago: old, wantMarked: 1},
		"its MBID is indexed and present under another name":   {ago: old, arrange: present(true, "mbid-nowhere", "renamed.flac")},
		"its MBID is present under another name: never marked": {arrange: present(true, "mbid-nowhere", "renamed.flac")},
		"its MBID is indexed under no available root": {ago: old, arrange: sqlArrange(`INSERT INTO scan_results (library_id, file_path, artist, title, status, recording_mbid)
			SELECT id, '/unmounted/renamed.flac', 'Artist', 'Title', 'done', 'mbid-nowhere' FROM libraries`)},
		"one present file has its title, and the row is unavailable": {ago: old, arrange: titled("unavailable")},
		"one present file has its title, and the row is failed":      {ago: old, arrange: titled("failed")},
		"file is back": {ago: old, arrange: func(t *testing.T, _ context.Context, _ *sql.DB, _ int64, track string) func(string) (fs.FileInfo, error) {
			return present(false, "", filepath.Base(track))(t, nil, nil, 0, track)
		}},
		// The configured root is an empty mountpoint; the files are elsewhere.
		"library root unavailable: not examined, the mark stands": {ago: old, wantMarked: 1, arrange: func(t *testing.T, ctx context.Context, sqlDB *sql.DB, _ int64, _ string) func(string) (fs.FileInfo, error) {
			return sqlArrange(`UPDATE libraries SET path = ?`, t.TempDir())(t, ctx, sqlDB, 0, "")
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
				t.Errorf("backup record written for %q", r.SourcePath)
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Pruned) != 0 || len(res.Relinked) != tc.relinked {
				t.Errorf("pruned=%d relinked=%d, want 0/%d", len(res.Pruned), len(res.Relinked), tc.relinked)
			}
			if sr, wq, _ := rowCounts(t, ctx, sqlDB); wq != 1 || sr < 1 {
				t.Errorf("scan_results=%d work_queue=%d, want the row kept", sr, wq)
			}
			if n := marked(t, ctx, sqlDB); n != tc.wantMarked {
				t.Errorf("marks = %d, want %d", n, tc.wantMarked)
			}
		})
	}
}

// A file that came back drops its mark, and one that goes again is marked
// afresh: the old mark is not inherited. A mark no row names is dropped too.
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
	at := time.Now().Add(time.Hour)
	later.now = func() time.Time { return at }
	if res := sweepDir(t, ctx, later); len(res.Pruned) != 0 || marked(t, ctx, sqlDB) != 1 || firstSeen(t, ctx, sqlDB, track) != at.Unix() {
		t.Fatalf("gone again: pruned=%d marks=%d, want 0/1 (a fresh mark)", len(res.Pruned), marked(t, ctx, sqlDB))
	}
	if _, wq, _ := rowCounts(t, ctx, sqlDB); wq != 2 {
		t.Fatalf("work_queue rows = %d, want 2", wq)
	}
}

// first_seen is never rewritten after insert, whatever the clock reads: not by
// a sweep under a clock that is behind (a boot before time sync), nor by one
// that is ahead. A mark later than now is not overdue.
func TestSweepDirectory_ClockNeverMovesTheMark(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	track := filepath.Join(root, "Artist", "Album", "01. a.mp3")
	seedRow(t, ctx, sqlDB, libID, track, "done", "done")
	seedRow(t, ctx, sqlDB, libID, filepath.Join(filepath.Dir(track), "02. b.mp3"), "done", "done")
	if err := os.Remove(track); err != nil {
		t.Fatal(err)
	}
	markGone(t, ctx, sqlDB, track, 24*time.Hour)
	want := firstSeen(t, ctx, sqlDB, track)
	for _, at := range []time.Duration{-6 * 365 * 24 * time.Hour, 6 * time.Hour, 365 * 24 * time.Hour} {
		age(t, filepath.Dir(track)) // examined each time, so the mark is offered again
		if _, err := sqlDB.ExecContext(ctx, `DELETE FROM prune_dir_state`); err != nil {
			t.Fatal(err)
		}
		p := New(sqlDB)
		p.now = func() time.Time { return time.Now().Add(at) }
		sweepDir(t, ctx, p)
		if _, wq, _ := rowCounts(t, ctx, sqlDB); wq != 2 || firstSeen(t, ctx, sqlDB, track) != want {
			t.Fatalf("clock %+v: work_queue rows = %d, first_seen = %d, want 2 and %d", at, wq, firstSeen(t, ctx, sqlDB, track), want)
		}
		if aging, due, err := p.AgingCounts(ctx); err != nil || aging+due != 1 || (due == 1) != (at > goneGrace) {
			t.Fatalf("clock %+v: aging=%d due=%d err=%v", at, aging, due, err)
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
	if _, wq, _ := rowCounts(t, ctx, sqlDB); wq != 2 {
		t.Fatalf("work_queue rows = %d, want 2", wq)
	}
}
