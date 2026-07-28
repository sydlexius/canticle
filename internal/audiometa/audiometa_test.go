package audiometa_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/audiometa"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/scanner"
)

// testReaderVersion is the duration-parser identity the single-Store tests write
// and read under. Its value is irrelevant; what matters is that it is CONSTANT,
// so those tests exercise mtime/size validation without reader identity
// confounding the result.
const testReaderVersion = "test-reader@v1"

func newTestDB(t *testing.T) *audiometa.Store {
	t.Helper()
	sqlDB, _ := newTestDBWithHandle(t, testReaderVersion)
	return sqlDB
}

// newTestDBWithHandle returns a Store reading under readerVersion plus the
// underlying handle, so a test can open a SECOND Store over the SAME database
// under a different identity -- the shape a parser swap takes in production,
// where the rows persist and the code changes underneath them.
func newTestDBWithHandle(t *testing.T, readerVersion string) (*audiometa.Store, *sql.DB) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return audiometa.New(sqlDB, readerVersion), sqlDB
}

func TestRecordThenLookupHitsWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	s := newTestDB(t)

	facts := scanner.AudioFacts{
		MTimeNano: 1234567890, SizeBytes: 4096,
		MBID: "mb-uuid", ISRC: "USRC12345678",
		Artist: "A", Title: "T", Album: "Al", Genre: "G", Year: 1997,
		TrackNo: 3, TrackTotal: 12, DiscNo: 1, DiscTotal: 2,
		Format: "ID3v2.4", FileType: "MP3", TrackLength: 210,
	}
	if err := s.Record(ctx, "/lib/a.mp3", facts); err != nil {
		t.Fatalf("Record: %v", err)
	}

	found, err := s.Lookup(ctx, "/lib/a.mp3", 1234567890, 4096)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Error("Lookup found = false, want true for an unchanged file")
	}
}

func TestLookupMissesWhenFileChanged(t *testing.T) {
	ctx := context.Background()
	s := newTestDB(t)
	if err := s.Record(ctx, "/lib/a.mp3", scanner.AudioFacts{MTimeNano: 1000, SizeBytes: 4096}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Same size, mtime moved by one nanosecond: a same-second rewrite to the
	// same byte size must still read as changed.
	found, err := s.Lookup(ctx, "/lib/a.mp3", 1001, 4096)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if found {
		t.Error("Lookup found = true for a changed file; staleness check is not working")
	}
}

func TestLookupMissesWhenAbsent(t *testing.T) {
	ctx := context.Background()
	s := newTestDB(t)
	found, err := s.Lookup(ctx, "/lib/never-seen.mp3", 1, 1)
	if err != nil {
		t.Fatalf("an absent row must not be an error: %v", err)
	}
	if found {
		t.Error("Lookup found = true for an absent row")
	}
}

func TestRecordUpsertsInPlace(t *testing.T) {
	ctx := context.Background()
	s := newTestDB(t)
	if err := s.Record(ctx, "/lib/a.mp3", scanner.AudioFacts{MTimeNano: 1, SizeBytes: 1, Genre: "Old"}); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if err := s.Record(ctx, "/lib/a.mp3", scanner.AudioFacts{MTimeNano: 2, SizeBytes: 2, Genre: "New"}); err != nil {
		t.Fatalf("second Record: %v", err)
	}

	var n int
	sqlDB := s.DB()
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM audio_metadata WHERE file_path='/lib/a.mp3'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 (Record must upsert, not accumulate)", n)
	}
	var genre string
	if err := sqlDB.QueryRow(`SELECT genre FROM audio_metadata WHERE file_path='/lib/a.mp3'`).Scan(&genre); err != nil {
		t.Fatalf("select genre: %v", err)
	}
	if genre != "New" {
		t.Errorf("genre = %q, want %q (upsert must overwrite)", genre, "New")
	}
}

func TestAbsentAndEmptySurviveRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestDB(t)
	// Only identity set: every descriptive field must round-trip as its sentinel.
	if err := s.Record(ctx, "/lib/bare.mp3", scanner.AudioFacts{MTimeNano: 5, SizeBytes: 6}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var genre, mbid string
	var year int
	sqlDB := s.DB()
	err := sqlDB.QueryRow(`SELECT genre, mbid, year FROM audio_metadata WHERE file_path='/lib/bare.mp3'`).Scan(&genre, &mbid, &year)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if genre != "" || mbid != "" || year != 0 {
		t.Errorf("sentinels = %q/%q/%d, want empty/empty/0", genre, mbid, year)
	}
}

// TestRecordRoundTripsEveryColumnDistinctly closes a gap left by the other
// tests here: they only ever assert genre/mbid/year round-trip, so a
// transposition among the other 15 columns (e.g. Artist and AlbumArtist
// swapped, or DiscNo and DiscTotal swapped) would compile and pass every
// existing test. Every field below is given a DISTINCT, recognizable value --
// two fields sharing a value would make a swap between them undetectable --
// and every column is read back and checked against the field that should
// have produced it.
func TestRecordRoundTripsEveryColumnDistinctly(t *testing.T) {
	ctx := context.Background()
	s := newTestDB(t)

	facts := scanner.AudioFacts{
		MTimeNano:   111111,
		SizeBytes:   222222,
		MBID:        "mbid-value",
		ISRC:        "isrc-value",
		Artist:      "artist-value",
		AlbumArtist: "album-artist-value",
		Title:       "title-value",
		Album:       "album-value",
		Composer:    "composer-value",
		Genre:       "genre-value",
		Year:        1991,
		TrackNo:     3,
		TrackTotal:  12,
		DiscNo:      2,
		DiscTotal:   4,
		Format:      "format-value",
		FileType:    "filetype-value",
		TrackLength: 333,
	}
	if err := s.Record(ctx, "/lib/full.mp3", facts); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var got struct {
		mtimeNsec, sizeBytes                                           int64
		mbid, isrc, artist, albumArtist, title, album, composer, genre string
		format, fileType                                               string
		year, trackNo, trackTotal, discNo, discTotal, durationSeconds  int
	}
	err := s.DB().QueryRow(`
		SELECT mtime_nsec, size_bytes, mbid, isrc, artist, album_artist, title, album,
		       composer, genre, year, track_no, track_total, disc_no, disc_total,
		       format, file_type, duration_seconds
		FROM audio_metadata WHERE file_path = '/lib/full.mp3'`).Scan(
		&got.mtimeNsec, &got.sizeBytes, &got.mbid, &got.isrc, &got.artist, &got.albumArtist,
		&got.title, &got.album, &got.composer, &got.genre, &got.year, &got.trackNo,
		&got.trackTotal, &got.discNo, &got.discTotal, &got.format, &got.fileType, &got.durationSeconds,
	)
	if err != nil {
		t.Fatalf("select: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"mtime_nsec", got.mtimeNsec, facts.MTimeNano},
		{"size_bytes", got.sizeBytes, facts.SizeBytes},
		{"mbid", got.mbid, facts.MBID},
		{"isrc", got.isrc, facts.ISRC},
		{"artist", got.artist, facts.Artist},
		{"album_artist", got.albumArtist, facts.AlbumArtist},
		{"title", got.title, facts.Title},
		{"album", got.album, facts.Album},
		{"composer", got.composer, facts.Composer},
		{"genre", got.genre, facts.Genre},
		{"year", got.year, facts.Year},
		{"track_no", got.trackNo, facts.TrackNo},
		{"track_total", got.trackTotal, facts.TrackTotal},
		{"disc_no", got.discNo, facts.DiscNo},
		{"disc_total", got.discTotal, facts.DiscTotal},
		{"format", got.format, facts.Format},
		{"file_type", got.fileType, facts.FileType},
		{"duration_seconds", got.durationSeconds, facts.TrackLength},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("column %s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestCoverageCountsIndexedRows(t *testing.T) {
	ctx := context.Background()
	s := newTestDB(t)
	for _, p := range []string{"/lib/a.mp3", "/lib/b.mp3", "/lib/c.mp3"} {
		if err := s.Record(ctx, p, scanner.AudioFacts{MTimeNano: 1, SizeBytes: 1}); err != nil {
			t.Fatalf("Record %s: %v", p, err)
		}
	}
	n, err := s.Coverage(ctx, "")
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if n != 3 {
		t.Errorf("Coverage = %d, want 3", n)
	}
}

// TestCoverageScopesByPathPrefix proves a --library scoped coverage count
// includes only rows under that root, and that the prefix comparison is not a
// SQL LIKE pattern match: one of the roots' paths contains a literal
// underscore, which is a LIKE wildcard and would over-match "My.Album" style
// paths (any single character) if the predicate were a naive LIKE without
// ESCAPE. Finding 3.
func TestCoverageScopesByPathPrefix(t *testing.T) {
	ctx := context.Background()
	s := newTestDB(t)

	rootA := "/library/Root_A/"
	rootB := "/library/RootB/"
	for _, p := range []string{rootA + "My_Album/a.mp3", rootA + "b.mp3"} {
		if err := s.Record(ctx, p, scanner.AudioFacts{MTimeNano: 1, SizeBytes: 1}); err != nil {
			t.Fatalf("Record %s: %v", p, err)
		}
	}
	if err := s.Record(ctx, rootB+"c.mp3", scanner.AudioFacts{MTimeNano: 1, SizeBytes: 1}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// A decoy row that a naive unescaped LIKE '/library/Root_A/%' predicate
	// would wrongly match, since '_' matches any single character in SQL LIKE:
	// "Root_A" would also match "RootXA". A correct, non-pattern comparison
	// must exclude it from rootA's count.
	decoy := "/library/RootXA/decoy.mp3"
	if err := s.Record(ctx, decoy, scanner.AudioFacts{MTimeNano: 1, SizeBytes: 1}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	n, err := s.Coverage(ctx, rootA)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if n != 2 {
		t.Errorf("Coverage(%q) = %d, want 2 (rootB row must not be counted)", rootA, n)
	}

	n, err = s.Coverage(ctx, rootB)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if n != 1 {
		t.Errorf("Coverage(%q) = %d, want 1", rootB, n)
	}
}

// THE #713 GUARANTEE. The file is byte-identical -- same path, same mtime, same
// size -- and the row is still stale, because the DURATION PARSER changed.
// audio_metadata.duration_seconds comes from the same parse as audio_durations
// (#711), so a swap that invalidated only that table would leave this one
// silently holding a mix of pre- and post-swap durations.
//
// Measured on the swap this test was written for: a VBR MP3 with no Xing header
// read 25.05s under the old parser and 60.03s under the new one, a 2.4x error
// in the direction that makes a correct lyric look wildly out of sync.
func TestReaderVersionChangeIsAMiss(t *testing.T) {
	ctx := context.Background()
	oldReader, sqlDB := newTestDBWithHandle(t, "parser@v1")
	newReader := audiometa.New(sqlDB, "parser@v2")

	facts := scanner.AudioFacts{
		MTimeNano: 100, SizeBytes: 200,
		Artist: "A", Title: "T", TrackLength: 25,
	}
	if err := oldReader.Record(ctx, "/music/vbr.mp3", facts); err != nil {
		t.Fatalf("Record under the old parser: %v", err)
	}

	found, err := newReader.Lookup(ctx, "/music/vbr.mp3", 100, 200)
	if err != nil {
		t.Fatalf("Lookup under the new parser: %v", err)
	}
	if found {
		t.Fatal("a row whose duration came from a DIFFERENT parser must read as a miss, so the file is re-read rather than trusted")
	}

	// The recording parser still hits: the row is invalidated for the NEW
	// reader, not destroyed. That is what makes the swap lazy rather than
	// a flag day.
	if found, err := oldReader.Lookup(ctx, "/music/vbr.mp3", 100, 200); err != nil {
		t.Fatalf("Lookup under the old parser: %v", err)
	} else if !found {
		t.Fatal("the recording parser must still hit its own row")
	}
}

// A re-read under the new parser must RE-STAMP the row rather than leave it
// unreadable. file_path is the conflict target, so without carrying
// reader_version into the upsert the row would miss forever and every scan
// would re-read the file -- a permanent I/O leak on an array kept spun down.
func TestRecordRestampsRowOnReaderChange(t *testing.T) {
	ctx := context.Background()
	oldReader, sqlDB := newTestDBWithHandle(t, "parser@v1")
	newReader := audiometa.New(sqlDB, "parser@v2")

	base := scanner.AudioFacts{MTimeNano: 100, SizeBytes: 200, Artist: "A", Title: "T"}

	stale := base
	stale.TrackLength = 25 // what the old parser derived
	if err := oldReader.Record(ctx, "/music/vbr.mp3", stale); err != nil {
		t.Fatalf("Record under the old parser: %v", err)
	}

	fresh := base
	fresh.TrackLength = 60 // what the new parser derives from the same bytes
	if err := newReader.Record(ctx, "/music/vbr.mp3", fresh); err != nil {
		t.Fatalf("Record under the new parser: %v", err)
	}

	if found, err := newReader.Lookup(ctx, "/music/vbr.mp3", 100, 200); err != nil {
		t.Fatalf("Lookup under the new parser: %v", err)
	} else if !found {
		t.Fatal("the re-read row must be readable under the new parser, else it re-reads forever")
	}

	// One row per file, re-stamped in place -- the old identity is gone.
	if found, err := oldReader.Lookup(ctx, "/music/vbr.mp3", 100, 200); err != nil {
		t.Fatalf("Lookup under the old parser: %v", err)
	} else if found {
		t.Fatal("the superseded parser must no longer hit")
	}

	// And the stored duration is the NEW parser's, not the stale one.
	var secs int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT duration_seconds FROM audio_metadata WHERE file_path=?`,
		"/music/vbr.mp3").Scan(&secs); err != nil {
		t.Fatalf("read back duration: %v", err)
	}
	if secs != 60 {
		t.Fatalf("duration_seconds = %d, want 60: the re-read must overwrite the stale parser's value", secs)
	}
}

// Legacy rows -- written before the column existed -- hold NULL and must read as
// a miss for EVERY reader, the empty string included. This is the no-backfill
// argument: NULL = ? is NULL rather than true, so the pre-existing population
// invalidates itself with no row data migrated.
func TestLegacyNullReaderVersionIsAMiss(t *testing.T) {
	ctx := context.Background()
	_, sqlDB := newTestDBWithHandle(t, "parser@v1")

	// Write the row the way pre-#713 code did: no reader_version at all.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO audio_metadata (file_path, mtime_nsec, size_bytes, duration_seconds)
		 VALUES (?, ?, ?, ?)`,
		"/music/legacy.mp3", 100, 200, 25,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	for _, readerVersion := range []string{"parser@v1", ""} {
		found, err := audiometa.New(sqlDB, readerVersion).Lookup(ctx, "/music/legacy.mp3", 100, 200)
		if err != nil {
			t.Fatalf("Lookup as %q: %v", readerVersion, err)
		}
		if found {
			t.Fatalf("a legacy NULL-reader row must miss for reader %q; the SQL NULL comparison is what invalidates the pre-existing population", readerVersion)
		}
	}
}

// Coverage must count only rows THIS reader would use. Without the
// reader_version predicate the first run after a parser swap reports FULL
// coverage immediately before re-reading the entire library -- exactly backwards
// as an operator signal, and the only signal that a swap is in flight (#713).
//
// This test exists because the predicate shipped UNTESTED: removing it from
// both Coverage branches left the whole suite green.
func TestCoverageCountsOnlyCurrentReader(t *testing.T) {
	ctx := context.Background()
	oldReader, sqlDB := newTestDBWithHandle(t, "parser@v1")
	newReader := audiometa.New(sqlDB, "parser@v2")

	for _, p := range []string{"/lib/a.mp3", "/lib/b.mp3", "/lib/c.mp3"} {
		if err := oldReader.Record(ctx, p, scanner.AudioFacts{
			MTimeNano: 100, SizeBytes: 200, Title: "T", TrackLength: 60,
		}); err != nil {
			t.Fatalf("Record %s: %v", p, err)
		}
	}

	// Sanity: the recording reader sees them, so a zero below is the predicate
	// working rather than an empty table.
	if n, err := oldReader.Coverage(ctx, ""); err != nil {
		t.Fatalf("Coverage under the recording reader: %v", err)
	} else if n != 3 {
		t.Fatalf("recording reader Coverage = %d, want 3", n)
	}

	// BOTH branches: the unprefixed count and the path-scoped one.
	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{"no prefix", ""},
		{"path prefix", "/lib/"},
	} {
		got, err := newReader.Coverage(ctx, tc.prefix)
		if err != nil {
			t.Fatalf("Coverage(%s): %v", tc.name, err)
		}
		if got != 0 {
			t.Errorf("Coverage(%s) = %d under a DIFFERENT reader, want 0: rows a swap will re-read must not read as covered", tc.name, got)
		}
	}
}
