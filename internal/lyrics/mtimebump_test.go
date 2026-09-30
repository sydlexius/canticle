package lyrics

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/selfwrite"
)

var bumpOld = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

// bumpFixture is a directory holding an audio file with a known old mtime.
type bumpFixture struct{ dir, audio string }

func newBumpFixture(t *testing.T) bumpFixture {
	t.Helper()
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac")
	seed(t, audio, "not really audio")
	if err := os.Chtimes(audio, bumpOld, bumpOld); err != nil {
		t.Fatal(err)
	}
	return bumpFixture{dir, audio}
}

func (f bumpFixture) bumped(t *testing.T) bool {
	t.Helper()
	fi, err := os.Stat(f.audio)
	if err != nil {
		t.Fatal(err)
	}
	return !fi.ModTime().Equal(bumpOld)
}

// write runs one WriteLRC of song against the fixture's audio.
func (f bumpFixture) write(t *testing.T, w *LRCWriter, song models.Song) error {
	t.Helper()
	song.AudioPath = f.audio
	return w.WriteLRC(song, "song.flac", f.dir)
}

func bumpWriter(enabled bool) *LRCWriter {
	w := NewLRCWriter()
	w.SetAudioMtimeBump(enabled)
	return w
}

func TestBump_FiresOnInPlaceUpgrade(t *testing.T) {
	_, _, line := rungSongs()
	for _, tc := range []struct{ name, seedName, seedBody string }{
		{"txt to lrc", "song.txt", "old plain words\n"},
		{"lrc replaced with different content", "song.lrc", "[ar:A]\n[00:01.00]other line\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBumpFixture(t)
			seed(t, filepath.Join(f.dir, tc.seedName), tc.seedBody)
			before, _ := os.ReadFile(f.audio)
			reg := selfwrite.New(time.Minute)
			w := bumpWriter(true)
			w.SetSelfWriteRegistry(reg)
			if err := f.write(t, w, line); err != nil {
				t.Fatal(err)
			}
			if !f.bumped(t) {
				t.Fatal("audio mtime not bumped after an in-place correction")
			}
			if after, _ := os.ReadFile(f.audio); sha256.Sum256(after) != sha256.Sum256(before) {
				t.Error("audio CONTENT changed; only mtime may")
			}
			if !reg.Suppress(f.audio) {
				t.Error("audio path not recorded with the self-write registry; the watcher would rescan on the bump")
			}
		})
	}
}

func TestBump_DoesNotFire(t *testing.T) {
	instrumental, unsynced, line := rungSongs()
	t.Run("fresh write", func(t *testing.T) {
		f := newBumpFixture(t)
		if err := f.write(t, bumpWriter(true), line); err != nil || f.bumped(t) {
			t.Errorf("first-ever write: err=%v bumped=%v", err, f.bumped(t))
		}
	})
	t.Run("fresh instrumental marker", func(t *testing.T) {
		f := newBumpFixture(t)
		if err := f.write(t, bumpWriter(true), instrumental); err != nil || f.bumped(t) {
			t.Errorf("fresh marker: err=%v bumped=%v", err, f.bumped(t))
		}
	})
	t.Run("kept better", func(t *testing.T) {
		f := newBumpFixture(t)
		seed(t, filepath.Join(f.dir, "song.lrc"), "[00:01.00]settled\n")
		if err := f.write(t, bumpWriter(true), unsynced); !errors.Is(err, ErrKeptBetter) {
			t.Fatalf("err = %v, want ErrKeptBetter", err)
		}
		if f.bumped(t) {
			t.Error("bumped on a refused write")
		}
	})
	t.Run("no-op rewrite", func(t *testing.T) {
		f := newBumpFixture(t)
		w := bumpWriter(true)
		first := line
		first.FetchedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		second := first
		second.FetchedAt = first.FetchedAt.Add(time.Hour) // header differs, lyrics do not
		if err := f.write(t, w, first); err != nil {
			t.Fatal(err)
		}
		if err := f.write(t, w, second); err != nil || f.bumped(t) {
			t.Errorf("rewrite with unchanged lyrics: err=%v bumped=%v", err, f.bumped(t))
		}
	})
	t.Run("key off", func(t *testing.T) {
		f := newBumpFixture(t)
		seed(t, filepath.Join(f.dir, "song.txt"), "old\n")
		w := bumpWriter(false)
		called := false
		w.chtimes = func(string, time.Time, time.Time) error { called = true; return nil }
		if err := f.write(t, w, line); err != nil || f.bumped(t) || called {
			t.Errorf("key off: err=%v bumped=%v chtimes called=%v", err, f.bumped(t), called)
		}
	})
	t.Run("no audio path", func(t *testing.T) {
		f := newBumpFixture(t)
		seed(t, filepath.Join(f.dir, "song.txt"), "old\n")
		if err := bumpWriter(true).WriteLRC(line, "song.flac", f.dir); err != nil || f.bumped(t) {
			t.Errorf("no AudioPath: err=%v bumped=%v", err, f.bumped(t))
		}
	})
}

func TestBump_FailureIsNonFatal(t *testing.T) {
	_, _, line := rungSongs()
	f := newBumpFixture(t)
	seed(t, filepath.Join(f.dir, "song.txt"), "old\n")
	w := bumpWriter(true)
	var gotAtime time.Time
	w.chtimes = func(_ string, a, _ time.Time) error {
		gotAtime = a
		return errors.New("read-only filesystem")
	}
	if err := f.write(t, w, line); err != nil {
		t.Fatalf("a failed bump failed the write: %v", err)
	}
	if !gotAtime.IsZero() {
		t.Error("atime not passed as zero; os.Chtimes would overwrite the access time")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "song.lrc")); err != nil {
		t.Errorf("sidecar missing after a failed bump: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "song.txt")); err == nil {
		t.Error("stale .txt survived; the failed bump rolled back the write")
	}
	// A missing audio file is equally non-fatal.
	seed(t, filepath.Join(f.dir, "song.txt"), "old again\n")
	if err := os.Remove(f.audio); err != nil {
		t.Fatal(err)
	}
	if err := f.write(t, w, line); err != nil {
		t.Fatalf("missing audio failed the write: %v", err)
	}
}

// TestBump_OnlyTheSidecarsOwnAudio: a multi-output row hands the writer one
// AudioPath for several sidecars; only the sidecar beside that file, with its
// stem, may bump it.
func TestBump_OnlyTheSidecarsOwnAudio(t *testing.T) {
	_, _, line := rungSongs()
	t.Run("different stem", func(t *testing.T) {
		f := newBumpFixture(t)
		seed(t, filepath.Join(f.dir, "other.txt"), "old\n")
		song := line
		song.AudioPath = f.audio
		if err := bumpWriter(true).WriteLRC(song, "other.flac", f.dir); err != nil || f.bumped(t) {
			t.Errorf("another stem's sidecar bumped this audio: err=%v bumped=%v", err, f.bumped(t))
		}
	})
	t.Run("different directory", func(t *testing.T) {
		f := newBumpFixture(t)
		out := t.TempDir()
		seed(t, filepath.Join(out, "song.txt"), "old\n")
		song := line
		song.AudioPath = f.audio
		if err := bumpWriter(true).WriteLRC(song, "song.flac", out); err != nil || f.bumped(t) {
			t.Errorf("a sidecar in another directory bumped this audio: err=%v bumped=%v", err, f.bumped(t))
		}
	})
	t.Run("matching sidecar still bumps", func(t *testing.T) {
		f := newBumpFixture(t)
		seed(t, filepath.Join(f.dir, "song.txt"), "old\n")
		if err := f.write(t, bumpWriter(true), line); err != nil || !f.bumped(t) {
			t.Errorf("matching sidecar: err=%v bumped=%v", err, f.bumped(t))
		}
	})
}

// TestLyricBody_BracketInHeaderValue: an ID-tag value containing ']' is still a
// header, so a header-only change is not a lyric correction.
func TestLyricBody_BracketInHeaderValue(t *testing.T) {
	a := lyricBody("s.lrc", []byte("[al:Album [Deluxe]]\n[00:01.00]same\n"))
	b := lyricBody("s.lrc", []byte("[al:Album [Deluxe Edition]]\n[00:01.00]same\n"))
	if a != b || a != "[00:01.00]same" {
		t.Errorf("bodies %q vs %q; want header stripped and equal", a, b)
	}
}

// TestLyricBody_TxtKeepsSectionAnnotations: in a .txt a bracketed annotation is
// lyric text, so changing it is a correction, while the writer's own
// provenance headers are still stripped.
func TestLyricBody_TxtKeepsSectionAnnotations(t *testing.T) {
	a := lyricBody("s.txt", []byte("[Chorus: A]\nwords\n"))
	b := lyricBody("s.txt", []byte("[Chorus: B]\nwords\n"))
	if a == b {
		t.Errorf("section annotation change read as unchanged: %q", a)
	}
	m1 := lyricBody("s.TXT", []byte("[by:canticle]\n[source:canticle-detector]\n[dv:1]\nInstrumental\n"))
	m2 := lyricBody("s.TXT", []byte("[by:canticle]\n[source:canticle-detector]\n[dv:2]\nInstrumental\n"))
	if m1 != m2 || m1 != "Instrumental" {
		t.Errorf("marker headers not stripped: %q vs %q", m1, m2)
	}
}
