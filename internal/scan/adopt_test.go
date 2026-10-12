package scan_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/sidecar"
)

const (
	lineLRC  = "[00:01.00]alpha beta\n[00:02.00]gamma delta\n"
	wordLRC  = "[00:01.00]<00:01.00>alpha <00:01.50>beta\n[00:02.00]gamma delta\n"
	plainLRC = "alpha beta without any timestamps\n"
	plainTXT = "alpha beta\ngamma delta\n"
	instTXT  = "♪ Instrumental ♪\n"
)

var adoptNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// adoptEnv is a real-SQLite fixture with counting seams over the classifiers.
type adoptEnv struct {
	t       *testing.T
	sqlDB   *sql.DB
	dbPath  string
	repo    *scan.Repo
	q       *queue.DBQueue
	lib     models.Library
	root    string
	reads   int
	records []queue.AdoptRecord
}

func newAdoptEnv(t *testing.T) *adoptEnv {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := dbtest.OpenAt(context.Background(), t, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	root := t.TempDir()
	lib, err := library.New(sqlDB).Add(context.Background(), root, "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("add library: %v", err)
	}
	return &adoptEnv{t: t, sqlDB: sqlDB, dbPath: dbPath, repo: scan.New(sqlDB), q: queue.NewDBQueue(sqlDB), lib: lib, root: root}
}

// seed writes an audio file (and its lyric file if lyricName is set) and
// indexes the audio as a settled 'done' scan row.
func (e *adoptEnv) seed(stem, artist, title, lyricName, lyric string) {
	e.t.Helper()
	e.seedIn(e.root, stem, artist, title, lyricName, lyric)
}

// seedIn is seed in an existing directory.
func (e *adoptEnv) seedIn(dir, stem, artist, title, lyricName, lyric string) {
	e.t.Helper()
	audio := filepath.Join(dir, stem+".mp3")
	if err := os.WriteFile(audio, []byte("audio"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	if lyricName != "" {
		if err := os.WriteFile(filepath.Join(dir, lyricName), []byte(lyric), 0o600); err != nil {
			e.t.Fatal(err)
		}
	}
	err := e.repo.Upsert(context.Background(), e.lib.ID, []models.ScanResult{{
		FilePath: audio, Track: models.Track{ArtistName: artist, TrackName: title},
		Outdir: dir, Filename: stem + ".lrc", Status: scan.StatusDone,
	}}, scan.UpsertOptions{})
	if err != nil {
		e.t.Fatalf("seed scan row: %v", err)
	}
}

func (e *adoptEnv) adopter(budget int) *scan.Adopter {
	return &scan.Adopter{
		Store: e.q, Now: func() time.Time { return adoptNow }, Budget: budget,
		Backup: func(r queue.AdoptRecord) error { e.records = append(e.records, r); return nil },
		List:   sidecar.List,
		ClassifyLRC: func(p string) (lyrics.SyncTier, error) {
			e.reads++
			return lyrics.ClassifyLRCFile(p)
		},
		RungOnDisk: func(fp string, l sidecar.Listing) lyrics.Rung {
			e.reads++
			return lyrics.RungOnDisk(fp, l)
		},
	}
}

func (e *adoptEnv) run(a *scan.Adopter) scan.AdoptCounts {
	e.t.Helper()
	n, err := a.Run(context.Background(), e.lib.ID, "")
	if err != nil {
		e.t.Fatalf("Run: %v", err)
	}
	return n
}

type queueRow struct {
	id                           int64
	status, outcome              string
	tier, lane, upstream, source sql.NullString
	attempts, links              int
	completed                    string
}

func (e *adoptEnv) row(artist, title string) (queueRow, bool) {
	e.t.Helper()
	ak, tk := queue.IdentityKeys(models.Track{ArtistName: artist, TrackName: title})
	var r queueRow
	var outcome sql.NullString
	err := e.sqlDB.QueryRow(`SELECT id, status, outcome_type, sync_tier, provider_lane, upstream, source_path, attempts, COALESCE(completed_at, ''),
        (SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = work_queue.id)
        FROM work_queue WHERE artist_key = ? AND title_key = ?`, ak, tk).
		Scan(&r.id, &r.status, &outcome, &r.tier, &r.lane, &r.upstream, &r.source, &r.attempts, &r.completed, &r.links)
	if errors.Is(err, sql.ErrNoRows) {
		return queueRow{}, false
	}
	if err != nil {
		e.t.Fatalf("read queue row: %v", err)
	}
	r.outcome = outcome.String
	return r, true
}

func (e *adoptEnv) queueCount() int {
	var n int
	if err := e.sqlDB.QueryRow(`SELECT COUNT(*) FROM work_queue`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestAdoptClassifiesEachSidecarFromDisk(t *testing.T) {
	e := newAdoptEnv(t)
	cases := []struct {
		artist, title, lyric, content string
		outcome, tier                 string
	}{
		{"Zebra Quartz", "Line Song", "line.lrc", lineLRC, "synced", "line"},
		{"Zebra Quartz", "Word Song", "word.lrc", wordLRC, "synced", "word"},
		{"Zebra Quartz", "Untimed Lrc", "untimed.lrc", plainLRC, "synced", "unsynced"},
		{"Zebra Quartz", "Text Song", "text.txt", plainTXT, "unsynced", ""},
		{"Zebra Quartz", "Marker Song", "marker.txt", instTXT, "instrumental", ""},
	}
	for _, c := range cases {
		stem := c.lyric[:len(c.lyric)-4]
		e.seed(stem, c.artist, c.title, c.lyric, c.content)
	}
	e.seed("bare", "Zebra Quartz", "Bare Song", "", "") // no lyric file: skipped, never read
	n := e.run(e.adopter(0))
	if n.Inserted != len(cases) || n.NoSidecar != 1 {
		t.Fatalf("counts = %+v, want %d inserted and one no-sidecar skip", n, len(cases))
	}
	for _, c := range cases {
		r, ok := e.row(c.artist, c.title)
		if !ok {
			t.Fatalf("%s: no queue row adopted", c.title)
		}
		if r.status != "done" || r.outcome != c.outcome || r.tier.String != c.tier || r.tier.Valid != (c.tier != "") {
			t.Errorf("%s: status=%q outcome=%q tier=%+v, want done/%s/%q", c.title, r.status, r.outcome, r.tier, c.outcome, c.tier)
		}
		if r.lane.Valid || r.upstream.Valid || r.attempts != 0 || r.links != 1 || !r.source.Valid || r.source.String == "" {
			t.Errorf("%s: lane=%+v upstream=%+v attempts=%d links=%d source=%+v; want no lane/upstream, 0 attempts, 1 link, a source_path", c.title, r.lane, r.upstream, r.attempts, r.links, r.source)
		}
		if r.completed != adoptNow.Format("2006-01-02T15:04:05Z") {
			t.Errorf("%s: completed_at = %q, want the adoption time", c.title, r.completed)
		}
	}
	if len(e.records) != len(cases) {
		t.Errorf("backup records = %d, want %d", len(e.records), len(cases))
	}
	// Adopted rows leave the query; a second pass re-skips only the bare row and reads and writes nothing.
	reads := e.reads
	if n := e.run(e.adopter(0)); n != (scan.AdoptCounts{NoSidecar: 1}) || e.reads != reads || len(e.records) != len(cases) || e.queueCount() != len(cases) {
		t.Errorf("second pass counts=%+v extra reads=%d records=%d rows=%d, want only the no-sidecar skip", n, e.reads-reads, len(e.records), e.queueCount())
	}
}

// A song whose queue row is not done is never linked (RecheckRetired would
// revive the settled file through the link) and nothing is read.
func TestAdoptLinksOnlyToASettledRowAndReadsNothingOtherwise(t *testing.T) {
	e := newAdoptEnv(t)
	statuses := map[string]string{"Busy Song": "processing", "Pending Song": "pending", "Gone Song": "unavailable"}
	for title, status := range statuses {
		stem := "s" + title[:1]
		e.seed(stem, "Zebra Quartz", title, stem+".lrc", lineLRC)
		ak, tk := queue.IdentityKeys(models.Track{ArtistName: "Zebra Quartz", TrackName: title})
		if _, err := e.sqlDB.Exec(`INSERT INTO work_queue (artist, title, artist_key, title_key, status) VALUES ('Zebra Quartz', ?, ?, ?, ?)`, title, ak, tk, status); err != nil {
			t.Fatal(err)
		}
	}
	a := e.adopter(0)
	n := e.run(a)
	if n.NotSettled != 3 || n.Inserted+n.Linked != 0 || e.reads != 0 || len(e.records) != 0 {
		t.Fatalf("counts = %+v reads=%d records=%d, want three not-settled skips and nothing read or written", n, e.reads, len(e.records))
	}
	for title, status := range statuses {
		if r, _ := e.row("Zebra Quartz", title); r.links != 0 || r.status != status {
			t.Errorf("%s row touched: links=%d status=%q", title, r.links, r.status)
		}
	}
	if _, err := e.sqlDB.Exec(`UPDATE work_queue SET status = 'done' WHERE title = 'Gone Song'`); err != nil {
		t.Fatal(err)
	}
	if n := e.run(a); n.Linked != 1 || n.NotSettled != 2 || e.reads != 0 {
		t.Errorf("after one row settled counts = %+v reads=%d, want a link and no read", n, e.reads)
	}
}

func TestAdoptedRowsAndTheUpgradeSweep(t *testing.T) {
	e := newAdoptEnv(t)
	e.seed("line", "Zebra Quartz", "Line Song", "line.lrc", lineLRC)
	e.seed("word", "Zebra Quartz", "Word Song", "word.lrc", wordLRC)
	e.seed("untimed", "Zebra Quartz", "Untimed Lrc", "untimed.lrc", plainLRC)
	e.seed("text", "Zebra Quartz", "Text Song", "text.txt", plainTXT)
	e.run(e.adopter(0))
	id := func(title string) int64 { r, _ := e.row("Zebra Quartz", title); return r.id }

	ctx := context.Background()
	if got, err := e.q.ListUpgradeCandidates(ctx, adoptNow.Add(-time.Hour), 100); err != nil || len(got) != 0 {
		t.Fatalf("candidates inside the hold = %v, %v; want none", got, err)
	}
	got, err := e.q.ListUpgradeCandidates(ctx, adoptNow.Add(time.Hour), 100)
	want := map[int64]bool{id("Untimed Lrc"): true, id("Text Song"): true}
	if err != nil || len(got) != len(want) || !want[got[0]] || !want[got[len(got)-1]] {
		t.Fatalf("upgrade candidates = %v, %v; want the unsynced .lrc and the .txt only", got, err)
	}
	// Production options (word_recheck_sweep.go): no CompletedBefore, 7-day cooldown.
	const cooldown = 7 * 24 * time.Hour
	candidates := func(now time.Time) map[int64]bool {
		t.Helper()
		rc, err := e.q.ListWordRecheckCandidates(context.Background(), queue.WordRecheckOptions{
			Generation: 1, UnexaminedCheckedBefore: now.Add(-cooldown), StampChecked: true, Limit: 100,
		})
		m := map[int64]bool{}
		for _, g := range rc {
			m[g] = true
		}
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if got := candidates(adoptNow.Add(time.Hour)); len(got) != 0 {
		t.Errorf("word-recheck candidates right after adoption = %v, want none", got)
	}
	if got := candidates(adoptNow.Add(cooldown + time.Hour)); !got[id("Line Song")] {
		t.Errorf("word-recheck candidates after the cooldown = %v, want the line-synced row", got)
	}
	var state sql.NullString
	if err := e.sqlDB.QueryRow(`SELECT word_timing_state FROM work_queue WHERE id = ?`, id("Line Song")).Scan(&state); err != nil || state.Valid {
		t.Errorf("word_timing_state = %v, %v; want NULL (no verdict reached)", state, err)
	}
}

// A backup that cannot be written stops the pass at the first failure.
func TestAdoptBackupFailureStopsThePass(t *testing.T) {
	e := newAdoptEnv(t)
	for _, st := range []string{"a", "b", "c"} {
		e.seed(st, "Zebra Quartz", "Song "+st, st+".lrc", lineLRC)
	}
	calls := 0
	a := e.adopter(0)
	a.Backup = func(queue.AdoptRecord) error { calls++; return errors.New("disk full") }
	n, err := a.Run(context.Background(), e.lib.ID, "")
	if !errors.Is(err, queue.ErrAdoptBackup) || n.WriteFailed != 1 || calls != 1 || e.reads != 1 || e.queueCount() != 0 {
		t.Errorf("err=%v counts=%+v calls=%d reads=%d rows=%d, want ErrAdoptBackup, stop after the first failure, database unchanged", err, n, calls, e.reads, e.queueCount())
	}
}

func TestAdoptBudgetStopsAtTheCapAndNextPassContinues(t *testing.T) {
	e := newAdoptEnv(t)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		e.seed(s, "Zebra Quartz", "Song "+s, s+".lrc", lineLRC)
	}
	a := e.adopter(2)
	first := e.run(a)
	if first.Inserted != 2 || !first.BudgetHit || e.reads != 2 {
		t.Fatalf("pass 1 = %+v reads=%d, want 2 adopted, budget hit, 2 reads", first, e.reads)
	}
	if second := e.run(a); second.Inserted != 2 || !second.BudgetHit {
		t.Fatalf("pass 2 = %+v, want 2 more", second)
	}
	if third := e.run(a); third.Inserted != 1 || third.BudgetHit || e.queueCount() != 5 {
		t.Fatalf("pass 3 = %+v rows=%d, want the last one and all 5 adopted", third, e.queueCount())
	}
}

// /x/ab is not under /x/a (whole path component), with or without a trailing separator.
func TestAdoptPathPrefixDoesNotMatchASiblingDirectory(t *testing.T) {
	for _, suffix := range []string{"", string(filepath.Separator)} {
		e := newAdoptEnv(t)
		for _, d := range []string{"a", "ab"} {
			if err := os.Mkdir(filepath.Join(e.root, d), 0o700); err != nil {
				t.Fatal(err)
			}
			e.seedIn(filepath.Join(e.root, d), "song", "Zebra Quartz", "Song "+d, "song.lrc", lineLRC)
		}
		n, err := e.adopter(0).Run(context.Background(), e.lib.ID, filepath.Join(e.root, "a")+suffix)
		if err != nil || n.Inserted != 1 {
			t.Fatalf("prefix %q: counts=%+v err=%v, want only the file under a/", suffix, n, err)
		}
		if _, ok := e.row("Zebra Quartz", "Song ab"); ok {
			t.Errorf("prefix %q adopted the sibling directory ab/", suffix)
		}
	}
}

// forcedStore returns a set result from LinkSettled or InsertSettledDone
// without touching the database (a race the real store produces only between
// two writers); an unset one delegates to the real queue.
type forcedStore struct {
	*queue.DBQueue
	link, insert *queue.AdoptResult
}

func (s *forcedStore) LinkSettled(ctx context.Context, c queue.AdoptCandidate, now time.Time, b func(queue.AdoptRecord) error) (queue.AdoptResult, error) {
	if s.link != nil {
		return *s.link, nil
	}
	return s.DBQueue.LinkSettled(ctx, c, now, b)
}

func (s *forcedStore) InsertSettledDone(ctx context.Context, c queue.AdoptCandidate, cls queue.AdoptClass, now time.Time, b func(queue.AdoptRecord) error) (queue.AdoptResult, error) {
	if s.insert != nil {
		return *s.insert, nil
	}
	return s.DBQueue.InsertSettledDone(ctx, c, cls, now, b)
}

// Every store result has its own count. A LinkSettled result that is not NoRow
// ends the row there (no file read); an Insert result other than Inserted or
// Linked is counted, never treated as success.
func TestAdoptCountsEachStoreResultWithoutWrites(t *testing.T) {
	res := func(r queue.AdoptResult) *queue.AdoptResult { return &r }
	cases := []struct {
		name         string
		link, insert *queue.AdoptResult
		want         scan.AdoptCounts
		wantRead     bool
	}{
		{"link already linked", res(queue.AdoptAlreadyLinked), nil, scan.AdoptCounts{AlreadyLinked: 1}, false},
		{"link unknown", res(99), nil, scan.AdoptCounts{Unexpected: 1}, false},
		{"insert already linked", nil, res(queue.AdoptAlreadyLinked), scan.AdoptCounts{AlreadyLinked: 1}, true},
		{"insert not settled", nil, res(queue.AdoptNotSettled), scan.AdoptCounts{NotSettled: 1}, true},
		{"insert unknown", nil, res(99), scan.AdoptCounts{Unexpected: 1}, true},
		{"insert no row", nil, res(queue.AdoptNoRow), scan.AdoptCounts{Unexpected: 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newAdoptEnv(t)
			e.seed("a", "Zebra Quartz", "Mauve Lantern", "a.lrc", lineLRC)
			a := e.adopter(0)
			a.Store = &forcedStore{DBQueue: e.q, link: tc.link, insert: tc.insert}
			if n := e.run(a); n != tc.want || (e.reads > 0) != tc.wantRead || e.queueCount() != 0 || len(e.records) != 0 {
				t.Errorf("counts=%+v reads=%d rows=%d records=%d, want %+v read=%v and nothing written", n, e.reads, e.queueCount(), len(e.records), tc.want, tc.wantRead)
			}
		})
	}
}
