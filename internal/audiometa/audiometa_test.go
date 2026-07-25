package audiometa_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/audiometa"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/scanner"
)

func newTestDB(t *testing.T) *audiometa.Store {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return audiometa.New(sqlDB)
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
