package worker

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/audiometa"
	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
)

// Fixture values for the banked-row tests. They are distinctive so the privacy
// test can search a captured log for them without a coincidental match.
const (
	bankArtist   = "Zephyrine Quillfeather"
	bankTitle    = "Obsidian Marmalade Waltz"
	bankAlbum    = "Thistledown Ledger"
	bankFile     = "quillfeather-waltz.flac"
	bankISRC     = "QZXYZ2600042"
	bankSeconds  = 200
	bankComposer = "Ottoline Vex"
	bankGenre    = "Skiffle"
	bankYear     = 1987
)

// bankRig is a worker wired to the real audiometa.Store over a real SQLite
// database, with one real FLAC file on disk.
type bankRig struct {
	w     *Worker
	db    *sql.DB
	store *audiometa.Store
	path  string // the file as the queue row spells it
	key   string // the cache key the worker derives from path
	// reads counts tag reads that returned a result; a failed open on a
	// corrupted file is counted in failedReads instead.
	reads, failedReads int
}

func bankComments() map[string]string {
	return map[string]string{
		"ARTIST": bankArtist, "TITLE": bankTitle, "ALBUM": bankAlbum, "ISRC": bankISRC,
		"COMPOSER": bankComposer, "GENRE": bankGenre, "DATE": "1987",
		"TRACKNUMBER": "7", "DISCNUMBER": "2",
	}
}

func newBankRig(t *testing.T) *bankRig {
	t.Helper()
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, bankFile, 44100, 44100*bankSeconds, bankComments()); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sqlDB := dbtest.Open(t)
	r := &bankRig{db: sqlDB, store: audiometa.New(sqlDB, scanner.DurationReaderVersion), path: filepath.Join(dir, bankFile)}
	r.key = pathutil.CanonicalPath(r.path)
	r.w = New(nil, nil, nil, nil)
	r.w.SetMetadataCache(r.store)
	r.w.readFacts = func(p string) (scanner.AudioFacts, error) {
		facts, err := scanner.ReadAudioFacts(p)
		if err != nil {
			r.failedReads++
			return facts, err
		}
		r.reads++
		return facts, nil
	}
	return r
}

// attempt runs the fetch-time refresh the way RunOnce does for a queued row,
// which carries artist and title but neither duration nor ISRC.
func (r *bankRig) attempt() models.Track {
	item := queuedItem(r.path)
	return r.w.refreshRecordingIdentity(context.Background(), item, item.Inputs.Track)
}

func (r *bankRig) rowCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM audio_metadata`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func bankStat(t *testing.T, path string) (mtimeNano, size int64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return info.ModTime().UnixNano(), info.Size()
}

// A cache miss at fetch time must leave a row that is current for the file and
// stamped with the current tag-reader version, under the key the lookup uses.
func TestFetchTimeMissBanksACurrentRow(t *testing.T) {
	r := newBankRig(t)
	mtime, size := bankStat(t, r.path)

	track := r.attempt()
	if track.TrackLength != bankSeconds || track.ISRC != bankISRC {
		t.Fatalf("attempt resolved (%d, %q), want (%d, %q)", track.TrackLength, track.ISRC, bankSeconds, bankISRC)
	}
	if r.reads != 1 {
		t.Fatalf("file read %d times on the miss, want 1", r.reads)
	}

	var version int
	err := r.db.QueryRow(`SELECT tag_reader_version FROM audio_metadata WHERE file_path=? AND mtime_nsec=? AND size_bytes=?`,
		r.key, mtime, size).Scan(&version)
	if err != nil {
		t.Fatalf("no row banked under the canonical key with the file's stamp: %v", err)
	}
	if version != scanner.TagReaderVersion {
		t.Errorf("tag_reader_version = %d, want %d", version, scanner.TagReaderVersion)
	}
	id, found, err := r.store.Identity(context.Background(), r.key, mtime, size)
	if err != nil || !found {
		t.Fatalf("Identity found=%v err=%v, want a hit for the banked row", found, err)
	}
	if id.Artist != bankArtist || id.Title != bankTitle || id.ISRC != bankISRC || id.TrackLength != bankSeconds {
		t.Errorf("Identity = %+v, want the file's tags", id)
	}
}

// The banked row must hold everything the tag read returned. Record replaces
// every column, so a row built from only the fields the fetch uses would blank
// the rest while reading as current.
func TestFetchTimeMissBanksTheFullFacts(t *testing.T) {
	r := newBankRig(t)
	want, err := scanner.ReadAudioFacts(r.path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if want.Composer != bankComposer || want.Genre != bankGenre || want.Year != bankYear || want.TrackNo != 7 || want.DiscNo != 2 {
		t.Fatalf("fixture does not carry the non-fetch fields: %+v", want)
	}

	r.attempt()

	got, found, err := r.store.Facts(context.Background(), r.key, want.MTimeNano, want.SizeBytes)
	if err != nil || !found {
		t.Fatalf("Facts found=%v err=%v, want the banked row", found, err)
	}
	if got != want {
		t.Errorf("banked row differs from the full tag read\n got %+v\nwant %+v", got, want)
	}
}

// The second attempt on a row must not need the file. The audio is overwritten
// with same-size garbage and its mtime restored, so a tag read would fail; the
// duration and ISRC can only come from the row the first attempt banked.
func TestSecondAttemptResolvesFromTheBankedRow(t *testing.T) {
	r := newBankRig(t)
	mtime, size := bankStat(t, r.path)
	info, err := os.Stat(r.path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	r.attempt()

	if err := os.WriteFile(r.path, bytes.Repeat([]byte{0xA5}, int(size)), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if err := os.Chtimes(r.path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if m, s := bankStat(t, r.path); m != mtime || s != size {
		t.Fatalf("stamp did not survive the overwrite: (%d, %d), want (%d, %d)", m, s, mtime, size)
	}
	if _, err := scanner.ReadAudioFacts(r.path); err == nil {
		t.Fatal("the corrupted file still parses, so this test cannot tell a cache hit from a read")
	}

	r.reads, r.failedReads = 0, 0
	track := r.attempt()

	if track.TrackLength != bankSeconds || track.ISRC != bankISRC {
		t.Errorf("second attempt resolved (%d, %q), want (%d, %q) from the cache",
			track.TrackLength, track.ISRC, bankSeconds, bankISRC)
	}
	if r.reads != 0 || r.failedReads != 0 {
		t.Errorf("second attempt read the file (%d ok, %d failed), want no read", r.reads, r.failedReads)
	}
}

// A failed cache write must cost the attempt nothing, and its log line must not
// carry library metadata. The real store's error quotes the file path, so this
// fails if the error is ever logged as-is.
func TestFetchTimeBankFailureIsLoggedAndNotFatal(t *testing.T) {
	r := newBankRig(t)
	if _, err := r.db.Exec(`CREATE TRIGGER bank_refused BEFORE INSERT ON audio_metadata
		BEGIN SELECT RAISE(ABORT, 'write refused'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	// The store's own error is what a careless log line would print.
	rerr := r.store.Record(context.Background(), r.key, scanner.AudioFacts{})
	if rerr == nil || !strings.Contains(rerr.Error(), bankFile) {
		t.Fatalf("the store's write error is %v, want a failure that quotes the path", rerr)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	track := r.attempt()

	if track.TrackLength != bankSeconds || track.ISRC != bankISRC {
		t.Errorf("attempt resolved (%d, %q) after a failed cache write, want (%d, %q)",
			track.TrackLength, track.ISRC, bankSeconds, bankISRC)
	}
	if n := r.rowCount(t); n != 0 {
		t.Fatalf("%d rows written, want 0: the write did not fail", n)
	}
	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "metadata cache write failed") ||
		!strings.Contains(logged, "error_class=store") {
		t.Errorf("the failed write was not logged at Warn with its class; log:\n%s", logged)
	}
	for _, private := range []string{bankFile, filepath.Dir(r.key), filepath.Dir(r.path), bankArtist, bankTitle, bankAlbum} {
		if strings.Contains(logged, private) {
			t.Errorf("log carries library metadata %q:\n%s", private, logged)
		}
	}
}

// The row's (mtime, size) must be the stamp of the handle the tags were read
// from. Here the file is replaced after the read and before the write: a row
// stamped from a path stat at write time would describe the replacement while
// holding the old file's tags, and would then be served for it.
func TestFetchTimeBankStampsTheHandleThatWasRead(t *testing.T) {
	r := newBankRig(t)
	oldMtime, oldSize := bankStat(t, r.path)
	replacement := testutil.GenerateFLACExtended(44100, 44100*90, map[string]string{"ARTIST": "Someone Else", "TITLE": "Another Song"})
	r.w.readFacts = func(p string) (scanner.AudioFacts, error) {
		facts, err := scanner.ReadAudioFacts(p)
		if werr := os.WriteFile(p, replacement, 0o644); werr != nil {
			t.Errorf("replace: %v", werr)
		}
		return facts, err
	}

	r.attempt()

	newMtime, newSize := bankStat(t, r.path)
	if newSize == oldSize {
		t.Fatal("the replacement has the original's size, so the two stamps cannot be told apart")
	}
	if _, found, err := r.store.Identity(context.Background(), r.key, newMtime, newSize); err != nil || found {
		t.Errorf("Identity for the REPLACEMENT found=%v err=%v, want a miss: the banked tags are the old file's", found, err)
	}
	id, found, err := r.store.Identity(context.Background(), r.key, oldMtime, oldSize)
	if err != nil || !found || id.Artist != bankArtist {
		t.Errorf("Identity for the file that was read: %+v found=%v err=%v, want its own tags", id, found, err)
	}
}

// A file that changes between the lookup's stat and the read is not banked: the
// bytes read are not provably the version either stamp names. The attempt still
// uses what it read.
func TestFetchTimeBankSkipsAFileThatChangedBeforeTheRead(t *testing.T) {
	r := newBankRig(t)
	replacement := testutil.GenerateFLACExtended(44100, 44100*90, map[string]string{"ISRC": "QZXYZ2600099"})
	r.w.statFile = func(p string) (int64, int64, error) {
		m, s, err := statFileIdentity(p)
		if werr := os.WriteFile(p, replacement, 0o644); werr != nil {
			t.Errorf("replace: %v", werr)
		}
		return m, s, err
	}

	oldMtime, oldSize := bankStat(t, r.path)

	meta, err := r.w.resolveMetadata(context.Background(), r.path)
	if err != nil {
		t.Fatalf("resolveMetadata: %v", err)
	}

	if meta.TrackLength != 90 || meta.ISRC != "QZXYZ2600099" {
		t.Errorf("attempt resolved (%d, %q), want the values of the file it read", meta.TrackLength, meta.ISRC)
	}
	if n := r.rowCount(t); n != 0 {
		t.Errorf("%d rows banked for a file that changed under the read, want 0", n)
	}
	// The stamp handed back is the read handle's, not the lookup's stat:
	// recordDuration banks the duration under it, so the lookup's stamp would
	// file the new file's duration under the old file's identity.
	newMtime, newSize := bankStat(t, r.path)
	if newSize == oldSize {
		t.Fatal("the replacement has the original's size, so the two stamps cannot be told apart")
	}
	if meta.MTimeNano != newMtime || meta.SizeBytes != newSize {
		t.Errorf("returned stamp (%d, %d), want the read handle's (%d, %d); the lookup saw (%d, %d)",
			meta.MTimeNano, meta.SizeBytes, newMtime, newSize, oldMtime, oldSize)
	}
}

// A failed lookup says nothing about the row, so the file is read and the cache
// is left alone; a reader installed with SetMetadataReader returns only three of
// the row's columns, so it never feeds a write either.
func TestFetchTimeBankOnlyOnACleanMissWithTheFullReader(t *testing.T) {
	full := func(string) (scanner.AudioFacts, error) {
		return scanner.AudioFacts{MTimeNano: 100, SizeBytes: 200, TrackLength: 210, ISRC: "FULL", Composer: "C"}, nil
	}

	lookupFailed := &fakeMetaCache{err: errors.New("database is locked")}
	w, _ := newCacheTestWorker(t, lookupFailed, 100, 200)
	w.readFacts = full
	if _, err := w.resolveMetadata(context.Background(), "/music/song.mp3"); err != nil {
		t.Fatalf("resolveMetadata: %v", err)
	}
	if len(lookupFailed.recorded) != 0 {
		t.Errorf("a failed lookup wrote %d row(s), want none", len(lookupFailed.recorded))
	}

	miss := &fakeMetaCache{}
	w, _ = newCacheTestWorker(t, miss, 100, 200)
	w.readFacts = full
	meta, err := w.resolveMetadata(context.Background(), "/music/song.mp3")
	if err != nil {
		t.Fatalf("resolveMetadata: %v", err)
	}
	if len(miss.recorded) != 1 || miss.recorded[0].Composer != "C" || miss.recordedKey != miss.gotPath {
		t.Errorf("a clean miss recorded %+v under %q, want the full read under the lookup key %q",
			miss.recorded, miss.recordedKey, miss.gotPath)
	}
	if meta.ISRC != "FULL" || meta.MTimeNano != 100 || meta.SizeBytes != 200 {
		t.Errorf("miss returned %+v, want the full reader's values and stamp", meta)
	}

	custom := &fakeMetaCache{}
	w = New(nil, nil, nil, nil)
	w.SetMetadataCache(custom)
	w.statFile = func(string) (int64, int64, error) { return 100, 200, nil }
	w.SetMetadataReader(func(string) (scanner.AudioMetadata, error) {
		return scanner.AudioMetadata{TrackLength: 5, MTimeNano: 100, SizeBytes: 200}, nil
	})
	if meta, err := w.resolveMetadata(context.Background(), "/music/song.mp3"); err != nil || meta.TrackLength != 5 {
		t.Fatalf("resolveMetadata = %+v, %v; want the injected reader's values", meta, err)
	}
	if len(custom.recorded) != 0 {
		t.Errorf("a three-field reader fed %d cache write(s), want none", len(custom.recorded))
	}
}

// A read whose duration parse failed is returned to the attempt and not banked:
// the failure may be transient, and a row stamped current would pin duration 0
// until the file changed. The next read, which parses, banks as usual.
func TestFetchTimeBankSkipsAReadWhoseDurationParseFailed(t *testing.T) {
	cache := &fakeMetaCache{}
	w, _ := newCacheTestWorker(t, cache, 100, 200)
	durationErr := errors.New("read frames: input/output error")
	w.readFacts = func(string) (scanner.AudioFacts, error) {
		return scanner.AudioFacts{MTimeNano: 100, SizeBytes: 200, ISRC: "FULL", Album: "A", DurationErr: durationErr}, nil
	}

	meta, err := w.resolveMetadata(context.Background(), "/music/song.mp3")
	if err != nil {
		t.Fatalf("resolveMetadata: %v", err)
	}
	want := scanner.AudioMetadata{ISRC: "FULL", AlbumName: "A", MTimeNano: 100, SizeBytes: 200}
	if meta != want {
		t.Errorf("read with a duration error returned %+v, want %+v", meta, want)
	}
	if len(cache.recorded) != 0 {
		t.Fatalf("a read whose duration parse failed wrote %d row(s), want none", len(cache.recorded))
	}

	w.readFacts = func(string) (scanner.AudioFacts, error) {
		return scanner.AudioFacts{MTimeNano: 100, SizeBytes: 200, ISRC: "FULL", Album: "A", TrackLength: 210}, nil
	}
	if _, err := w.resolveMetadata(context.Background(), "/music/song.mp3"); err != nil {
		t.Fatalf("resolveMetadata: %v", err)
	}
	if len(cache.recorded) != 1 || cache.recorded[0].TrackLength != 210 {
		t.Errorf("the later successful read recorded %+v, want one row with the parsed duration", cache.recorded)
	}
}

// A read whose Vorbis multi-value re-read failed is returned to the attempt and
// not banked: its artist is a fallback that a row stamped current would pin.
func TestFetchTimeBankSkipsAReadWhoseMultiValueReadFailed(t *testing.T) {
	cache := &fakeMetaCache{}
	w, _ := newCacheTestWorker(t, cache, 100, 200)
	facts := scanner.AudioFacts{MTimeNano: 100, SizeBytes: 200, TrackLength: 210, MultiValueErr: scanner.ErrVorbisMultiValue}
	w.readFacts = func(string) (scanner.AudioFacts, error) { return facts, nil }
	meta, err := w.resolveMetadata(context.Background(), "/music/song.flac")
	if err != nil || meta.TrackLength != 210 || len(cache.recorded) != 0 {
		t.Fatalf("failed re-read: meta %+v, err %v, %d row(s) written; want the read returned and no row", meta, err, len(cache.recorded))
	}
	facts.MultiValueErr = nil
	if _, err := w.resolveMetadata(context.Background(), "/music/song.flac"); err != nil || len(cache.recorded) != 1 {
		t.Errorf("clean read: err %v, %d row(s) written; want one", err, len(cache.recorded))
	}
}

// The production wiring: a worker built by New with only SetMetadataCache, and
// no field replaced, must bank a miss through RunOnce. Every other test here
// swaps readFacts for a counting wrapper, so without this one New could lose
// its default and nothing would notice.
func TestRunOnceBanksAMissWithTheDefaultReader(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, bankFile, 44100, 44100*bankSeconds, bankComments()); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	path := filepath.Join(dir, bankFile)
	store := audiometa.New(dbtest.Open(t), scanner.DurationReaderVersion)
	q := &fakeQueue{items: []queue.WorkItem{queuedItem(path)}}
	f := refreshFetcher()

	w := New(q, &fakeCache{}, f, &fakeWriter{})
	w.SetMetadataCache(store)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(f.tracks) != 1 || f.tracks[0].TrackLength != bankSeconds || f.tracks[0].ISRC != bankISRC {
		t.Fatalf("provider saw %+v, want one track carrying the file's duration and ISRC", f.tracks)
	}
	mtime, size := bankStat(t, path)
	id, found, err := store.Identity(context.Background(), pathutil.CanonicalPath(path), mtime, size)
	if err != nil || !found {
		t.Fatalf("Identity found=%v err=%v, want the row RunOnce banked to be current", found, err)
	}
	if id.Artist != bankArtist || id.Title != bankTitle {
		t.Errorf("Identity = %+v, want the file's tags", id)
	}
}

// The row is stored under the symlink-resolved path, whatever spelling the
// queue row carries. The symlink is explicit because a temp dir is itself
// behind one only on macOS; on Linux nothing else here tells the lookup key
// from the queued path.
func TestFetchTimeBankKeysOnTheResolvedPath(t *testing.T) {
	r := newBankRig(t)
	realDir, err := filepath.EvalSymlinks(filepath.Dir(r.path))
	if err != nil {
		t.Fatalf("resolve fixture dir: %v", err)
	}
	link := filepath.Join(t.TempDir(), "library")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	r.path = filepath.Join(link, bankFile)
	resolved := filepath.Join(realDir, bankFile)
	if resolved == r.path {
		t.Fatal("the queued path is already the resolved one, so the key cannot be told from it")
	}

	if track := r.attempt(); track.TrackLength != bankSeconds {
		t.Fatalf("attempt through the symlink resolved duration %d, want %d", track.TrackLength, bankSeconds)
	}

	var keys []string
	rows, err := r.db.Query(`SELECT file_path FROM audio_metadata`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(keys) != 1 || keys[0] != resolved {
		t.Errorf("banked under %q, want exactly the resolved path %q (queued as %q)", keys, resolved, r.path)
	}
}

// A write that failed because the context was canceled is shutdown, not a
// fault: it logs at Debug, still by class only.
func TestFetchTimeBankCanceledWriteLogsAtDebug(t *testing.T) {
	const path = "/music/quillfeather/song.mp3"
	cache := &fakeMetaCache{recordErr: fmt.Errorf("record %s: %w", path, context.Canceled)}
	w, _ := newCacheTestWorker(t, cache, 100, 200)
	w.readFacts = func(string) (scanner.AudioFacts, error) {
		return scanner.AudioFacts{MTimeNano: 100, SizeBytes: 200, TrackLength: 210, Artist: bankArtist, Title: bankTitle}, nil
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	meta, err := w.resolveMetadata(context.Background(), path)
	if err != nil || meta.TrackLength != 210 {
		t.Fatalf("resolveMetadata = %+v, %v; want the read values despite the canceled write", meta, err)
	}
	if len(cache.recorded) != 1 {
		t.Fatalf("Record called %d times, want 1: the write was never attempted", len(cache.recorded))
	}
	logged := buf.String()
	if !strings.Contains(logged, "level=DEBUG") || !strings.Contains(logged, "metadata cache write failed") ||
		!strings.Contains(logged, "error_class=canceled") {
		t.Errorf("the canceled write was not logged at Debug with its class; log:\n%s", logged)
	}
	if strings.Contains(logged, "level=WARN") {
		t.Errorf("a canceled write logged at Warn:\n%s", logged)
	}
	for _, private := range []string{path, "quillfeather", bankArtist, bankTitle} {
		if strings.Contains(logged, private) {
			t.Errorf("log carries library metadata %q:\n%s", private, logged)
		}
	}
}
