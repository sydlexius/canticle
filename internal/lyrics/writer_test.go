package lyrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
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
	// Instrumentals carry a provenance header (#502): [by:canticle] but no
	// [source:]/[dv:] since neither WinningLane nor DetectorVersion is set.
	const want = "[by:canticle]\n\u266a Instrumental \u266a\n"
	if content != want {
		t.Fatalf("expected content to equal %q, got: %q", want, content)
	}
	if strings.Contains(content, "[00:00.00]") {
		t.Fatalf("instrumental marker must not contain an LRC timestamp, got: %q", content)
	}
	if strings.Contains(content, "[ar:") || strings.Contains(content, "[ti:") {
		t.Fatalf("instrumental marker must not contain synced tag headers, got: %q", content)
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
	const want = "[by:canticle]\n♪ Instrumental ♪\n"
	if content != want {
		t.Fatalf("expected content to equal %q, got: %q", want, content)
	}
	if strings.Contains(content, "[00:00.00]") {
		t.Fatalf("instrumental marker must not contain an LRC timestamp, got: %q", content)
	}
	if strings.Contains(content, "[ar:") || strings.Contains(content, "[ti:") {
		t.Fatalf("instrumental marker must not contain synced tag headers, got: %q", content)
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

// TestWriteLRC_UnsyncedIsExactlyTheLyricBody pins the maintainer's decision from
// #620: provenance must NEVER appear in an unsynced lyric file. An unsynced .txt
// is plain lyric text that players render verbatim, so any header block would
// show up as the first lines of the displayed lyrics.
//
// This asserts the EXACT bytes rather than substrings, because that is the only
// form that catches the regression this guards against. Every other unsynced
// test here checks "contains the lyrics" and would keep passing if a
// [by:canticle]/[isrc:]/[ve:] block were prepended -- exactly the change this
// decision forbids. Completion provenance lives on the work_queue row instead
// (queue.SetCompletionProvenance); if you are here because you want a file to
// carry provenance, that is the mechanism, and this test is not the obstacle to
// route around.
func TestWriteLRC_UnsyncedIsExactlyTheLyricBody(t *testing.T) {
	w := NewLRCWriter()
	tmpDir := t.TempDir()

	body := "First line\nSecond line"
	// Every field that feeds the synced .lrc tag block is populated, so if the
	// unsynced branch ever started emitting tags there would be real values to
	// emit and the byte comparison would fail loudly.
	song := models.Song{
		Track: models.Track{
			ArtistName:    "Test Artist",
			TrackName:     "Lyric Track",
			AlbumName:     "Test Album",
			TrackLength:   215,
			ISRC:          "USABC1234567",
			RecordingMBID: "8f3471b5-7e6a-4d3f-9c21-0a1b2c3d4e5f",
		},
		Lyrics:      models.Lyrics{LyricsBody: body},
		WinningLane: "musixmatch",
		FetchedAt:   time.Date(2026, 7, 23, 12, 30, 0, 0, time.UTC),
	}

	if err := w.WriteLRC(song, "", tmpDir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}

	fp := filepath.Join(tmpDir, Slugify("Test Artist - Lyric Track")+".txt")
	data, err := os.ReadFile(fp) //nolint:gosec // test path constructed from known test data
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}

	// The body verbatim, with no trailing newline added: this is the writer's
	// current byte-exact output, captured so any change to it is deliberate.
	want := body
	if string(data) != want {
		t.Errorf("unsynced .txt bytes changed.\n got: %q\nwant: %q\n\nAn unsynced .txt must be the lyric body and nothing else (#620).", string(data), want)
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
	const want = "[by:canticle]\n♪ Instrumental ♪\n"
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

func TestWriteLRC_InstrumentalDetectorProvenance(t *testing.T) {
	dir := t.TempDir()
	w := NewLRCWriter()
	song := models.Song{
		Track:           models.Track{ArtistName: "A", TrackName: "T", Instrumental: 1},
		DetectorVersion: "9.9.9",
	}
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	got := readOnlyTxt(t, dir)
	for _, want := range []string{"[source:canticle-detector]", "[dv:9.9.9]", InstrumentalMarker} {
		if !strings.Contains(got, want) {
			t.Errorf("detector marker missing %q; got:\n%s", want, got)
		}
	}
}

func TestWriteLRC_InstrumentalProviderProvenance(t *testing.T) {
	dir := t.TempDir()
	w := NewLRCWriter()
	song := models.Song{
		Track:       models.Track{ArtistName: "A", TrackName: "T", Instrumental: 1},
		WinningLane: "musixmatch",
	}
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	got := readOnlyTxt(t, dir)
	if !strings.Contains(got, "[source:musixmatch]") {
		t.Errorf("provider marker missing [source:musixmatch]; got:\n%s", got)
	}
	if strings.Contains(got, "[dv:") {
		t.Errorf("provider marker must not carry [dv:]; got:\n%s", got)
	}
}

// readOnlyTxt returns the contents of the single .txt file written into dir.
func readOnlyTxt(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one .txt in %s, got %v (err %v)", dir, matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	return string(data)
}

// TestWriteLRC_UpstreamTag pins the [upstream:] tag (#859): the LICENSOR a
// multiplexing lane routed a result to, carried separately from [source:] (the
// LANE) so that purgeprovenance.provenanceAgrees holds unchanged and a
// --source filter on a first-party provider cannot sweep in files that lane
// merely ROUTED through it. See docs/provider-attribution.md.
func TestWriteLRC_UpstreamTag(t *testing.T) {
	// The two upstream tokens are deliberately spelled here rather than
	// imported from internal/innertube: this package must not depend on a
	// provider, and a test that imported the constant would pass even if the
	// constant changed to something the writer mangles. The literal is the
	// contract.
	for _, tc := range []struct {
		name     string
		lane     string
		upstream string
		want     string // the exact tag expected, or "" for none
	}{
		{"multiplexing lane, licensor named", "innertube", "musixmatch", "[upstream:musixmatch]"},
		{"multiplexing lane, other licensor", "innertube", "lyricfind", "[upstream:lyricfind]"},
		{"multiplexing lane, no licensor named", "innertube", "", ""},
		{"a lane that is its own upstream", "petitlyrics", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewLRCWriter()
			tmpDir := t.TempDir()
			song := models.Song{
				Track:       models.Track{ArtistName: "Test Artist", TrackName: "Test Track"},
				Subtitles:   models.Synced{Lines: []models.Lines{{Text: "placeholder", Time: models.Time{}}}},
				WinningLane: tc.lane,
				Upstream:    tc.upstream,
			}
			if err := w.WriteLRC(song, "", tmpDir); err != nil {
				t.Fatalf("WriteLRC: %v", err)
			}
			fp := filepath.Join(tmpDir, Slugify("Test Artist - Test Track")+".lrc")
			data, err := os.ReadFile(fp) //nolint:gosec // reason: test path built from known test data
			if err != nil {
				t.Fatalf("read sidecar: %v", err)
			}
			content := string(data)

			// [source:] is the LANE and must never vary with the upstream --
			// that invariance is what keeps provenanceAgrees true, so it is
			// asserted on every row rather than only where the tag appears.
			wantSource := "[source:" + tc.lane + "]"
			if !strings.Contains(content, wantSource) {
				t.Errorf("missing %q: the source tag must carry the LANE, got:\n%s", wantSource, content)
			}
			if tc.upstream != "" && strings.Contains(content, "[source:"+tc.upstream+"]") {
				t.Errorf("the upstream leaked into [source:]: a --source filter on %q would then match this lane's files, got:\n%s", tc.upstream, content)
			}

			if tc.want == "" {
				if strings.Contains(content, "[upstream:") {
					t.Errorf("expected NO upstream tag (an absent tag asserts nothing), got:\n%s", content)
				}
				return
			}
			if !strings.Contains(content, tc.want) {
				t.Errorf("expected %q, got:\n%s", tc.want, content)
			}

			// Round-trip: the tag must read back through the same parser a
			// consumer would use, not merely be present as a substring.
			pt, err := ReadProvenanceTags(fp)
			if err != nil {
				t.Fatalf("ReadProvenanceTags: %v", err)
			}
			if pt.Upstream != tc.upstream {
				t.Errorf("round-trip: wrote upstream %q, read back %q", tc.upstream, pt.Upstream)
			}
			if pt.Source != tc.lane {
				t.Errorf("round-trip: wrote source %q, read back %q", tc.lane, pt.Source)
			}
		})
	}
}

// TestWriteLRC_UpstreamNeverRidesADetectorVerdict pins that a detector-decided
// instrumental carries no [upstream:]. The detector is canticle's own; crediting
// a licensor for a call canticle made would be a false attribution, and the
// instrumental branch derives src from the detector rather than the lane.
func TestWriteLRC_UpstreamNeverRidesADetectorVerdict(t *testing.T) {
	w := NewLRCWriter()
	tmpDir := t.TempDir()
	song := models.Song{
		Track:           models.Track{ArtistName: "Test Artist", TrackName: "Detected Instrumental", Instrumental: 1},
		WinningLane:     DetectorLaneName,
		DetectorVersion: "test-model-1",
		// Set deliberately: a caller could carry an upstream from an earlier
		// provider attempt, and the detector branch must not adopt it.
		Upstream: "musixmatch",
	}
	if err := w.WriteLRC(song, "", tmpDir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	fp := filepath.Join(tmpDir, Slugify("Test Artist - Detected Instrumental")+".txt")
	data, err := os.ReadFile(fp) //nolint:gosec // reason: test path built from known test data
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "[source:"+SourceDetector+"]") {
		t.Fatalf("premise broken: expected a detector-sourced instrumental, got:\n%s", content)
	}
	if strings.Contains(content, "[upstream:") {
		t.Errorf("a detector verdict must carry NO upstream -- it is canticle's own call, not a licensor's, got:\n%s", content)
	}
}

// TestWriteLRC_ProviderAssertedInstrumentalCarriesUpstream is the other half:
// when a PROVIDER asserted the instrumental, src is still the lane and the
// upstream rides along. Without this, the guard above would be satisfied by a
// writer that never writes the tag in the instrumental branch at all.
func TestWriteLRC_ProviderAssertedInstrumentalCarriesUpstream(t *testing.T) {
	w := NewLRCWriter()
	tmpDir := t.TempDir()
	song := models.Song{
		Track:       models.Track{ArtistName: "Test Artist", TrackName: "Provider Instrumental", Instrumental: 1},
		WinningLane: "innertube",
		Upstream:    "lyricfind",
	}
	if err := w.WriteLRC(song, "", tmpDir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	fp := filepath.Join(tmpDir, Slugify("Test Artist - Provider Instrumental")+".txt")
	data, err := os.ReadFile(fp) //nolint:gosec // reason: test path built from known test data
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "[source:innertube]") {
		t.Fatalf("premise broken: expected the lane as source, got:\n%s", content)
	}
	if !strings.Contains(content, "[upstream:lyricfind]") {
		t.Errorf("a provider-asserted instrumental must carry its upstream, got:\n%s", content)
	}
}

// TestWriteLRC_UpstreamNeverAppearsWithoutSource is the regression test for the
// review's F1 finding, and the shape it pins is not hypothetical.
//
// Song.WinningLane and Song.Upstream are populated by DIFFERENT layers:
// WinningLane only by the orchestrator (serve mode), Upstream by the provider
// itself, one layer below. One-shot `fetch` mode never runs the orchestrator --
// providers.New returns a bare namedProvider that forwards to the client and
// stamps no lane -- so with providers.primary set to a multiplexing lane, a
// writer that guarded the two tags INDEPENDENTLY emitted [upstream:<licensor>]
// with no [source:] at all. Measured on disk before the fix.
//
// Why that file is wrong rather than merely incomplete: purge's --no-source
// cohort is documented as the inherited/foreign sidecars canticle never wrote,
// and it matched a file that positively names the licensor canticle fetched
// from. The two tags must therefore appear and disappear together.
func TestWriteLRC_UpstreamNeverAppearsWithoutSource(t *testing.T) {
	for _, tc := range []struct {
		name         string
		instrumental int
	}{
		{"synced result", 0},
		{"instrumental", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewLRCWriter()
			tmpDir := t.TempDir()
			song := models.Song{
				Track: models.Track{
					ArtistName:   "Test Artist",
					TrackName:    "Fetch Mode Track",
					Instrumental: tc.instrumental,
				},
				// WinningLane deliberately EMPTY: this is the fetch-mode shape.
				Upstream: "lyricfind",
			}
			if tc.instrumental == 0 {
				song.Subtitles = models.Synced{Lines: []models.Lines{{Text: "placeholder", Time: models.Time{}}}}
			}
			if err := w.WriteLRC(song, "", tmpDir); err != nil {
				t.Fatalf("WriteLRC: %v", err)
			}
			ext := ".lrc"
			if tc.instrumental == 1 {
				ext = ".txt"
			}
			fp := filepath.Join(tmpDir, Slugify("Test Artist - Fetch Mode Track")+ext)
			data, err := os.ReadFile(fp) //nolint:gosec // reason: test path built from known test data
			if err != nil {
				t.Fatalf("read sidecar: %v", err)
			}
			content := string(data)

			// The premise: no [source:] on this path. If a future change starts
			// stamping a lane in fetch mode, this Fatal says so rather than
			// letting the real assertion pass vacuously.
			if strings.Contains(content, "[source:") {
				t.Fatalf("test premise broken: this path is supposed to write no [source:], got:\n%s", content)
			}
			if strings.Contains(content, "[upstream:") {
				t.Errorf("wrote an ORPHAN [upstream:] with no [source:] beside it -- the file names a licensor while landing in purge's \"canticle never wrote this\" cohort. Got:\n%s", content)
			}
		})
	}
}
