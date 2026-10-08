package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/worker"
)

func blockedPlainSong() models.Song {
	return models.Song{
		Track:             models.Track{ArtistName: "Cached Artist", TrackName: "Cached Title"},
		Lyrics:            models.Lyrics{LyricsBody: "wrong words\nmore wrong words\n"},
		IdentityArtistKey: "artist", IdentityTitleKey: "title",
	}
}

// blockedSongFetcher answers every lookup with the song a block was added for.
type blockedSongFetcher struct{ song models.Song }

func (f blockedSongFetcher) FindLyrics(context.Context, models.Track) (models.Song, error) {
	return f.song, nil
}

// TestRunServe_InstallsLyricBlockCheckers proves the serve graph wires the
// block store into BOTH the LRC writer backstop and the worker (#1394): a body
// blocked in the database is refused by the writer, and the wired worker defers
// a queued row whose only answer is blocked instead of writing it. Fetch mode
// builds neither.
func TestRunServe_InstallsLyricBlockCheckers(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	t.Setenv("MXLRC_DOCKER", "")
	t.Setenv("MUSIXMATCH_TOKEN", "test-token") // a Musixmatch primary needs one; the fetcher is faked
	t.Setenv("MXLRC_SECRETS_KEY_FILE", filepath.Join(t.TempDir(), "test.key"))
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve.db")
	sqlDB, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	song := blockedPlainSong()
	if _, err := lyricblock.NewStore(sqlDB, nil).Add(context.Background(), sqlDB, lyricblock.Block{ArtistKey: "artist", TitleKey: "title", Fingerprint: lyricblock.SongFingerprints(song)[0]}); err != nil {
		t.Fatal(err)
	}
	// A queued row for the blocked track: the worker's own dispatch is what the
	// probe drives, so the test sees the checker only through its behavior.
	rowTrack := models.Track{ArtistName: "Artist", TrackName: "Title"}
	if ak, tk := queue.IdentityKeys(rowTrack); ak != "artist" || tk != "title" {
		t.Fatalf("row identity = %q/%q; the block above would not match it", ak, tk)
	}
	lrcName := "row.lrc"
	item, err := queue.NewDBQueue(sqlDB).Enqueue(context.Background(), models.Inputs{Track: rowTrack, Outdir: dir, Filename: lrcName}, 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()

	held, err := net.Listen("tcp", "127.0.0.1:0") // a held port makes runServe exit at the bind, after the wiring
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := "[db]\npath = " + tomlString(dbPath) + "\n\n[providers]\nprimary = \"musixmatch\"\nfallback_order = [\"musixmatch\"]\n\n[server]\naddr = " + tomlString(held.Addr().String()) + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// Asserted INSIDE the probe: runServe closes the database as it exits, and a
	// closed store fails open.
	var probed bool
	var writeErr error
	var rowStatus string
	var rowWritten bool
	serveWiringProbe = func(w *worker.Worker, wr lyrics.Writer) {
		probed = true
		writeErr = wr.WriteLRC(song, "x.lrc", dir)
		// Drop the writer's backstop so only the worker's own checker can refuse
		// the row; otherwise the backstop would mask a missing worker install.
		configureWriterBlocks(wr, nil)
		if err := w.RunOnce(context.Background()); err != nil {
			t.Errorf("worker RunOnce: %v", err)
		}
		_, statErr := os.Stat(filepath.Join(dir, lrcName))
		rowWritten = statErr == nil
		rowStatus = probeRowStatus(t, dbPath, item.ID)
	}
	t.Cleanup(func() { serveWiringProbe = func(*worker.Worker, lyrics.Writer) {} })

	var out bytes.Buffer
	_ = runServe(context.Background(), &out, ServeCmd{ConfigPath: cfgPath},
		func(string) musixmatch.Fetcher { return blockedSongFetcher{song: song} },
		func(...string) lyrics.Writer { return lyrics.NewLRCWriter() })
	if !probed {
		t.Fatal("runServe never reached the wiring probe")
	}
	if !errors.Is(writeErr, lyrics.ErrBlocked) {
		t.Fatalf("serve writer WriteLRC = %v; want ErrBlocked (writer backstop not wired)", writeErr)
	}
	if rowWritten || rowStatus != queue.StatusDeferred {
		t.Fatalf("worker row: written = %v, status = %q; want unwritten and %q (worker.SetBlockChecker not wired)", rowWritten, rowStatus, queue.StatusDeferred)
	}
}

// TestScheduler_BlockedCacheEntryReadsAsMiss proves scheduler() hands the scan
// enqueuer its block checker: a cached entry blocked for the scanned track is a
// miss, so the track is enqueued rather than marked done from the cache (#1394).
func TestScheduler_BlockedCacheEntryReadsAsMiss(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	lib, err := library.New(sqlDB).Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	track := models.Track{ArtistName: "Artist", TrackName: "Title"}
	if err := scan.New(sqlDB).Upsert(ctx, lib.ID, []models.ScanResult{{
		FilePath: "/music/a.mp3", Track: track, Outdir: "/music", Filename: "a.lrc", Status: scan.StatusPending,
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatal(err)
	}
	song := blockedPlainSong()
	enc, err := json.Marshal(song)
	if err != nil {
		t.Fatal(err)
	}
	cacheRepo := cache.New(sqlDB)
	if err := cacheRepo.Store(ctx, track.ArtistName, track.TrackName, 0, string(enc)); err != nil {
		t.Fatal(err)
	}
	if _, err := lyricblock.NewStore(sqlDB, nil).Add(ctx, sqlDB, lyricblock.Block{ArtistKey: track.ArtistName, TitleKey: track.TrackName, Fingerprint: lyricblock.SongFingerprints(song)[0]}); err != nil {
		t.Fatal(err)
	}

	s := scheduler(sqlDB, scanner.ScanOptions{}, nil, false, cacheRepo, nil, "", 0)
	if err := s.OnScanComplete(ctx, models.Library{ID: lib.ID}, nil, lib.Path, scan.TriggerScheduler); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("work_queue rows = %d (%v); want 1 (the blocked cache entry must read as a miss, not a hit)", rows, err)
	}
}

// probeRowStatus reads a work_queue row's status through a second handle, since
// the serve graph owns the main one while the probe runs.
func probeRowStatus(t *testing.T, dbPath string, id int64) string {
	t.Helper()
	d, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Errorf("open for status: %v", err)
		return ""
	}
	defer func() { _ = d.Close() }()
	var status string
	if err := d.QueryRow(`SELECT status FROM work_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Errorf("read row status: %v", err)
	}
	return status
}
