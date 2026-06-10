package lyrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

func TestWriteLRC_NothingToSave(t *testing.T) {
	w := NewLRCWriter()
	tmpDir := t.TempDir()

	song := models.Song{
		Track: models.Track{
			ArtistName:   "Test Artist",
			TrackName:    "Test Track",
			Instrumental: 0,
		},
		Lyrics:    models.Lyrics{LyricsBody: ""},
		Subtitles: models.Synced{Lines: nil},
	}

	err := w.WriteLRC(song, "", tmpDir)
	if err == nil {
		t.Fatal("expected error 'nothing to save', got nil")
	}

	// No file should have been created on disk
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading tmpDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no files in tmpDir, found %d: %v", len(entries), entries)
	}
}

func TestWriteLRC_Instrumental(t *testing.T) {
	w := NewLRCWriter()
	tmpDir := t.TempDir()

	song := models.Song{
		Track: models.Track{
			ArtistName:   "Test Artist",
			TrackName:    "Instrumental Track",
			Instrumental: 1,
		},
		Lyrics:    models.Lyrics{LyricsBody: ""},
		Subtitles: models.Synced{Lines: nil},
	}

	err := w.WriteLRC(song, "", tmpDir)
	if err != nil {
		t.Fatalf("expected nil error for instrumental, got: %v", err)
	}

	// Instrumentals are unsynced content and must be saved as .txt, not .lrc.
	fn := Slugify("Test Artist - Instrumental Track") + ".txt"
	fp := filepath.Join(tmpDir, fn)
	data, err := os.ReadFile(fp) //nolint:gosec // test path constructed from known test data
	if err != nil {
		t.Fatalf("expected file %s to exist: %v", fp, err)
	}
	content := string(data)
	if len(content) == 0 {
		t.Fatal("expected non-empty file content for instrumental")
	}
	// Instrumentals are a plain marker: the single line with no timestamp and no
	// tag headers.
	const want = "\u266a Instrumental \u266a\n"
	if content != want {
		t.Fatalf("expected content to equal %q, got: %q", want, content)
	}
	if strings.Contains(content, "[00:00.00]") {
		t.Fatalf("instrumental marker must not contain an LRC timestamp, got: %q", content)
	}
	if strings.Contains(content, "[by:") || strings.Contains(content, "[ar:") || strings.Contains(content, "[ti:") {
		t.Fatalf("instrumental marker must not contain tag headers, got: %q", content)
	}

	// No .lrc file should have been created for an instrumental.
	lrcFn := Slugify("Test Artist - Instrumental Track") + ".lrc"
	if _, err := os.Stat(filepath.Join(tmpDir, lrcFn)); err == nil {
		t.Fatal("expected no .lrc file for instrumental, but one was created")
	}
}

func TestWriteLRC_InstrumentalExplicitFilename(t *testing.T) {
	w := NewLRCWriter()
	tmpDir := t.TempDir()

	song := models.Song{
		Track: models.Track{
			ArtistName:   "Test Artist",
			TrackName:    "Instrumental Track",
			Instrumental: 1,
		},
	}

	// Dir mode passes an explicit .lrc filename; an instrumental must still be
	// written as .txt.
	if err := w.WriteLRC(song, "song.lrc", tmpDir); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	txtPath := filepath.Join(tmpDir, "song.txt")
	data, err := os.ReadFile(txtPath) //nolint:gosec // test path constructed from known test data
	if err != nil {
		t.Fatalf("expected file song.txt to exist: %v", err)
	}
	content := string(data)
	const want = "♪ Instrumental ♪\n"
	if content != want {
		t.Fatalf("expected content to equal %q, got: %q", want, content)
	}
	if strings.Contains(content, "[00:00.00]") {
		t.Fatalf("instrumental marker must not contain an LRC timestamp, got: %q", content)
	}
	if strings.Contains(content, "[by:") || strings.Contains(content, "[ar:") || strings.Contains(content, "[ti:") {
		t.Fatalf("instrumental marker must not contain tag headers, got: %q", content)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "song.lrc")); err == nil {
		t.Fatal("expected no .lrc file for instrumental, but one was created")
	}
}

func TestWriteLRC_Unsynced(t *testing.T) {
	w := NewLRCWriter()
	tmpDir := t.TempDir()

	song := models.Song{
		Track: models.Track{
			ArtistName: "Test Artist",
			TrackName:  "Lyric Track",
		},
		Lyrics:    models.Lyrics{LyricsBody: "Hello world\nGoodbye world"},
		Subtitles: models.Synced{Lines: nil},
	}

	err := w.WriteLRC(song, "", tmpDir)
	if err != nil {
		t.Fatalf("expected nil error for unsynced, got: %v", err)
	}

	// Unsynced lyrics must be saved as .txt, not .lrc.
	fn := Slugify("Test Artist - Lyric Track") + ".txt"
	fp := filepath.Join(tmpDir, fn)
	data, err := os.ReadFile(fp) //nolint:gosec // test path constructed from known test data
	if err != nil {
		t.Fatalf("expected file %s to exist: %v", fp, err)
	}

	// File must contain plain text -- no LRC timestamp prefix.
	content := string(data)
	if strings.Contains(content, "[00:00.00]") {
		t.Fatalf("unsynced .txt must not contain LRC timestamps, got: %q", content)
	}
	if !strings.Contains(content, "Hello world") {
		t.Fatalf("expected plain lyrics in .txt file, got: %q", content)
	}

	// No .lrc file should have been created.
	lrcFn := Slugify("Test Artist - Lyric Track") + ".lrc"
	if _, err := os.Stat(filepath.Join(tmpDir, lrcFn)); err == nil {
		t.Fatal("expected no .lrc file for unsynced lyrics, but one was created")
	}
}

func TestWriteLRC_UnsyncedExplicitFilename(t *testing.T) {
	w := NewLRCWriter()
	tmpDir := t.TempDir()

	song := models.Song{
		Track: models.Track{
			ArtistName: "Test Artist",
			TrackName:  "Lyric Track",
		},
		Lyrics:    models.Lyrics{LyricsBody: "Verse one"},
		Subtitles: models.Synced{Lines: nil},
	}

	// Simulates dir-mode where the scanner passes an explicit .lrc filename.
	// The writer should swap to .txt since content is unsynced.
	err := w.WriteLRC(song, "song.lrc", tmpDir)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}

	fp := filepath.Join(tmpDir, "song.txt")
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("expected file %s to exist: %v", fp, err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "song.lrc")); err == nil {
		t.Fatal("expected no .lrc file when content is unsynced")
	}
}

// TestWriteLRC_StaleSidecarCleanup verifies that WriteLRC removes the opposite
// sidecar file after a successful write so format transitions never leave both
// files on disk.
func TestWriteLRC_StaleSidecarCleanup(t *testing.T) {
	syncedSong := models.Song{
		Track: models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "Line", Time: models.Time{Minutes: 0, Seconds: 1, Hundredths: 0}},
		}},
	}
	unsyncedSong := models.Song{
		Track:  models.Track{ArtistName: "Artist", TrackName: "Track"},
		Lyrics: models.Lyrics{LyricsBody: "Plain lyrics"},
	}

	t.Run("writes_lrc_removes_stale_txt", func(t *testing.T) {
		dir := t.TempDir()
		// Pre-create a stale .txt for the same stem.
		staleTxt := filepath.Join(dir, "song.txt")
		if err := os.WriteFile(staleTxt, []byte("old unsynced"), 0o644); err != nil {
			t.Fatalf("creating stale .txt: %v", err)
		}

		w := NewLRCWriter()
		if err := w.WriteLRC(syncedSong, "song.lrc", dir); err != nil {
			t.Fatalf("WriteLRC: %v", err)
		}

		if _, err := os.Stat(staleTxt); !os.IsNotExist(err) {
			t.Errorf("expected stale .txt to be removed, but it still exists (err=%v)", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "song.lrc")); err != nil {
			t.Errorf("expected .lrc to exist: %v", err)
		}
	})

	t.Run("writes_lrc_no_stale_txt_is_ok", func(t *testing.T) {
		dir := t.TempDir()
		w := NewLRCWriter()
		// No pre-existing .txt -- must not error.
		if err := w.WriteLRC(syncedSong, "song.lrc", dir); err != nil {
			t.Fatalf("WriteLRC without stale .txt: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "song.lrc")); err != nil {
			t.Errorf("expected .lrc to exist: %v", err)
		}
	})

	t.Run("writes_txt_removes_stale_lrc", func(t *testing.T) {
		dir := t.TempDir()
		// Pre-create a stale .lrc for the same stem.
		staleLrc := filepath.Join(dir, "song.lrc")
		if err := os.WriteFile(staleLrc, []byte("[00:01.00]Old line\n"), 0o644); err != nil {
			t.Fatalf("creating stale .lrc: %v", err)
		}

		w := NewLRCWriter()
		if err := w.WriteLRC(unsyncedSong, "song.lrc", dir); err != nil {
			t.Fatalf("WriteLRC: %v", err)
		}

		if _, err := os.Stat(staleLrc); !os.IsNotExist(err) {
			t.Errorf("expected stale .lrc to be removed, but it still exists (err=%v)", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "song.txt")); err != nil {
			t.Errorf("expected .txt to exist: %v", err)
		}
	})

	t.Run("writes_txt_no_stale_lrc_is_ok", func(t *testing.T) {
		dir := t.TempDir()
		w := NewLRCWriter()
		// No pre-existing .lrc -- must not error.
		if err := w.WriteLRC(unsyncedSong, "song.lrc", dir); err != nil {
			t.Fatalf("WriteLRC without stale .lrc: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "song.txt")); err != nil {
			t.Errorf("expected .txt to exist: %v", err)
		}
	})

	t.Run("writes_lrc_both_pre_exist", func(t *testing.T) {
		dir := t.TempDir()
		// Both files exist before an upgrade write -- only .lrc should remain.
		if err := os.WriteFile(filepath.Join(dir, "song.lrc"), []byte("old lrc"), 0o644); err != nil {
			t.Fatalf("creating pre-existing .lrc: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "song.txt"), []byte("old txt"), 0o644); err != nil {
			t.Fatalf("creating pre-existing .txt: %v", err)
		}

		w := NewLRCWriter()
		if err := w.WriteLRC(syncedSong, "song.lrc", dir); err != nil {
			t.Fatalf("WriteLRC: %v", err)
		}

		if _, err := os.Stat(filepath.Join(dir, "song.lrc")); err != nil {
			t.Errorf("expected .lrc to exist: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "song.txt")); !os.IsNotExist(err) {
			t.Errorf("expected .txt to be removed, but it still exists (err=%v)", err)
		}
	})

	t.Run("writes_txt_both_pre_exist", func(t *testing.T) {
		dir := t.TempDir()
		// Both files exist before a downgrade write -- only .txt should remain.
		if err := os.WriteFile(filepath.Join(dir, "song.lrc"), []byte("old lrc"), 0o644); err != nil {
			t.Fatalf("creating pre-existing .lrc: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "song.txt"), []byte("old txt"), 0o644); err != nil {
			t.Fatalf("creating pre-existing .txt: %v", err)
		}

		w := NewLRCWriter()
		if err := w.WriteLRC(unsyncedSong, "song.lrc", dir); err != nil {
			t.Fatalf("WriteLRC: %v", err)
		}

		if _, err := os.Stat(filepath.Join(dir, "song.txt")); err != nil {
			t.Errorf("expected .txt to exist: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "song.lrc")); !os.IsNotExist(err) {
			t.Errorf("expected .lrc to be removed, but it still exists (err=%v)", err)
		}
	})
}

// TestWriteLRC_InstrumentalWithSubtitles covers the real Musixmatch response: an
// instrumental track carries both Track.Instrumental==1 AND a synced subtitle line.
// The instrumental flag must win -- output must be .txt, not .lrc.
func TestWriteLRC_InstrumentalWithSubtitles(t *testing.T) {
	w := NewLRCWriter()
	tmpDir := t.TempDir()

	song := models.Song{
		Track: models.Track{
			ArtistName:   "Test Artist",
			TrackName:    "Instrumental Track",
			Instrumental: 1,
		},
		Subtitles: models.Synced{Lines: []models.Lines{
			// Musixmatch delivers this synced line for instrumentals.
			{Text: "♪ Instrumental ♪", Time: models.Time{Minutes: 0, Seconds: 0, Hundredths: 0}},
		}},
	}

	if err := w.WriteLRC(song, "song.lrc", tmpDir); err != nil {
		t.Fatalf("expected nil error for instrumental with subtitles, got: %v", err)
	}

	txtPath := filepath.Join(tmpDir, "song.txt")
	data, err := os.ReadFile(txtPath) //nolint:gosec // test path constructed from known test data
	if err != nil {
		t.Fatalf("expected song.txt to exist: %v", err)
	}
	const want = "♪ Instrumental ♪\n"
	if got := string(data); got != want {
		t.Fatalf("content = %q; want %q", got, want)
	}
	if strings.Contains(string(data), "[00:00.00]") {
		t.Fatalf("instrumental must not contain LRC timestamp, got: %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "song.lrc")); err == nil {
		t.Fatal("expected no .lrc file for instrumental with subtitles, but one was created")
	}
}

func TestWriteLRC_Synced(t *testing.T) {
	w := NewLRCWriter()
	tmpDir := t.TempDir()

	song := models.Song{
		Track: models.Track{
			ArtistName: "Test Artist",
			TrackName:  "Synced Track",
		},
		Lyrics: models.Lyrics{},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "Line one", Time: models.Time{Minutes: 0, Seconds: 5, Hundredths: 10}},
		}},
	}

	err := w.WriteLRC(song, "", tmpDir)
	if err != nil {
		t.Fatalf("expected nil error for synced, got: %v", err)
	}

	fn := Slugify("Test Artist - Synced Track") + ".lrc"
	fp := filepath.Join(tmpDir, fn)
	data, err := os.ReadFile(fp) //nolint:gosec // test path constructed from known test data
	if err != nil {
		t.Fatalf("expected file %s to exist: %v", fp, err)
	}

	content := string(data)
	if !strings.Contains(content, "[00:05.10]Line one") {
		t.Fatalf("expected synced timestamp in .lrc, got: %q", content)
	}
	if !strings.Contains(content, "[ar:Test Artist]") {
		t.Fatalf("expected LRC tags in .lrc, got: %q", content)
	}
}
