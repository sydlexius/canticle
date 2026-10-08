package reports_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
)

func setSourcePath(t *testing.T, sqlDB *sql.DB, id int64, p string) {
	t.Helper()
	if _, err := sqlDB.ExecContext(context.Background(),
		"UPDATE work_queue SET source_path = ? WHERE id = ?", p, id); err != nil {
		t.Fatalf("set source_path: %v", err)
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func TestPreviewSourceSidecars(t *testing.T) {
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac")
	writeFile(t, audio, "a")
	writeFile(t, filepath.Join(dir, "song.lrc"), "[00:01.00]hi\n")
	writeFile(t, filepath.Join(dir, "song.elrc"), "[by:canticle]\n[00:01.00]hi\n")
	id := insertWorkItem(t, sqlDB, workItem{
		artist: "Ar", title: "Ti", album: "Al", status: "done", syncTier: "word",
	})
	setSourcePath(t, sqlDB, id, audio)

	got, err := repo.PreviewSource(context.Background(), id)
	if err != nil {
		t.Fatalf("PreviewSource: %v", err)
	}
	want := reports.PreviewTarget{
		ID: id, Artist: "Ar", Title: "Ti", Album: "Al", Status: "done", SyncTier: "word", ArtistKey: "Ar", TitleKey: "Ti",
		AudioPath:     audio,
		LRCPath:       filepath.Join(dir, "song.lrc"),
		ELRCCandidate: filepath.Join(dir, "song.elrc"),
	}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// A foreign companion is still the CANDIDATE: PreviewSource never opens the
// file, so it makes no ownership claim; the consumer's bounded header read
// is what rejects it.
func TestPreviewSourceForeignCompanionIsStillCandidate(t *testing.T) {
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac")
	writeFile(t, filepath.Join(dir, "song.lrc"), "[00:01.00]hi\n")
	writeFile(t, filepath.Join(dir, "song.elrc"), "[by:someone-else]\n")
	id := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "T", status: "done"})
	setSourcePath(t, sqlDB, id, audio)

	got, err := repo.PreviewSource(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "song.elrc"); got.LRCPath == "" || got.ELRCCandidate != want {
		t.Fatalf("lrc=%q elrc=%q: want lrc set, candidate %q", got.LRCPath, got.ELRCCandidate, want)
	}
	if got.SyncTier != "" {
		t.Fatalf("SyncTier = %q, want empty for NULL", got.SyncTier)
	}
}

func TestPreviewSourceMissingSidecarsAndSymlink(t *testing.T) {
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac")
	target := filepath.Join(dir, "elsewhere.txt")
	writeFile(t, target, "x")
	if err := os.Symlink(target, filepath.Join(dir, "song.lrc")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	id := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "T", status: "done"})
	setSourcePath(t, sqlDB, id, audio)

	got, err := repo.PreviewSource(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.AudioPath != audio || got.LRCPath != "" || got.ELRCCandidate != "" {
		t.Fatalf("got %+v: symlinked .lrc must not be reported", got)
	}
}

func TestPreviewSourceNoSourcePath(t *testing.T) {
	sqlDB := openTestDB(t)
	id := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "T", status: "pending"})
	got, err := reports.New(sqlDB).PreviewSource(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.AudioPath != "" || got.LRCPath != "" || got.ELRCCandidate != "" || got.Status != "pending" {
		t.Fatalf("got %+v", got)
	}
}

func TestPreviewSourceCaseVariantLRC(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "Probe.X")
	writeFile(t, probe, "x")
	if _, err := os.Lstat(filepath.Join(dir, "probe.x")); err == nil {
		t.Skip("case-insensitive filesystem: the exact Lstat hides the variant path (runs on Linux/CI only)")
	}
	sqlDB := openTestDB(t)
	audio := filepath.Join(dir, "song.flac")
	writeFile(t, audio, "a")
	writeFile(t, filepath.Join(dir, "song.LRC"), "[00:01.00]hi\n")
	id := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "T", status: "done"})
	setSourcePath(t, sqlDB, id, audio)
	got, err := reports.New(sqlDB).PreviewSource(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "song.LRC"); got.LRCPath != want {
		t.Fatalf("LRCPath = %q, want %q", got.LRCPath, want)
	}
}

// caseSensitive reports whether dir's filesystem keeps case-variant names
// distinct (false on default APFS, where the exact Lstat hides a variant).
func caseSensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "Probe.X")
	writeFile(t, probe, "x")
	defer func() { _ = os.Remove(probe) }()
	_, err := os.Lstat(filepath.Join(dir, "probe.x"))
	return err != nil
}

// previewELRC seeds song.flac + song.lrc in a fresh dir, lets seed shape the
// .elrc names, and returns the dir and the row's ELRCCandidate.
func previewELRC(t *testing.T, dir string, seed func(dir string)) string {
	t.Helper()
	sqlDB := openTestDB(t)
	audio := filepath.Join(dir, "song.flac")
	writeFile(t, audio, "a")
	writeFile(t, filepath.Join(dir, "song.lrc"), "[00:01.00]hi\n")
	seed(dir)
	id := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "T", status: "done"})
	setSourcePath(t, sqlDB, id, audio)
	got, err := reports.New(sqlDB).PreviewSource(context.Background(), id)
	if err != nil {
		t.Fatalf("PreviewSource: %v", err)
	}
	if got.LRCPath == "" {
		t.Fatalf("LRCPath empty; the .elrc probe never ran")
	}
	return got.ELRCCandidate
}

// The writer's exact-name rule (lyrics.OwnedCompanionOf): an exact stem.elrc
// that exists in any non-regular form decides on its own, so no variant is
// ever reported beside it.
func TestPreviewSourceExactELRCNameDecides(t *testing.T) {
	owned := "[by:canticle]\n[00:01.00]hi\n"

	t.Run("directory at exact name, no variant", func(t *testing.T) {
		got := previewELRC(t, t.TempDir(), func(dir string) {
			if err := os.Mkdir(filepath.Join(dir, "song.elrc"), 0o700); err != nil {
				t.Fatal(err)
			}
		})
		if got != "" {
			t.Fatalf("ELRCCandidate = %q, want none for a directory", got)
		}
	})

	for _, tc := range []struct {
		name string
		mk   func(t *testing.T, exact string)
	}{
		{"directory at exact name", func(t *testing.T, exact string) {
			if err := os.Mkdir(exact, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling symlink at exact name", func(t *testing.T, exact string) {
			if err := os.Symlink(filepath.Join(filepath.Dir(exact), "missing"), exact); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
		}},
	} {
		t.Run(tc.name+" beside an owned variant", func(t *testing.T) {
			dir := t.TempDir()
			if !caseSensitive(t, dir) {
				t.Skip("case-insensitive filesystem: song.elrc and song.ELRC cannot coexist (runs on Linux/CI only)")
			}
			got := previewELRC(t, dir, func(dir string) {
				tc.mk(t, filepath.Join(dir, "song.elrc"))
				writeFile(t, filepath.Join(dir, "song.ELRC"), owned)
			})
			if got != "" {
				t.Fatalf("ELRCCandidate = %q: the writer never pairs a variant beside an existing exact name", got)
			}
		})
	}

	t.Run("exact absent, variant reported", func(t *testing.T) {
		dir := t.TempDir()
		if !caseSensitive(t, dir) {
			t.Skip("case-insensitive filesystem: the exact Lstat hides the variant path (runs on Linux/CI only)")
		}
		got := previewELRC(t, dir, func(dir string) {
			writeFile(t, filepath.Join(dir, "song.ELRC"), owned)
		})
		if want := filepath.Join(dir, "song.ELRC"); got != want {
			t.Fatalf("ELRCCandidate = %q, want %q", got, want)
		}
	})

	t.Run("no elrc at all", func(t *testing.T) {
		if got := previewELRC(t, t.TempDir(), func(string) {}); got != "" {
			t.Fatalf("ELRCCandidate = %q, want none", got)
		}
	})
}

// PreviewSource opens nothing: an exact .elrc that would block an open (a
// FIFO) or refuse one (mode 0000) is still reported, with no error and no
// hang, because only Lstat ever touches it.
func TestPreviewSourceOpensNoSidecar(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode 0000")
	}
	t.Run("unreadable", func(t *testing.T) {
		got := previewELRC(t, t.TempDir(), func(dir string) {
			p := filepath.Join(dir, "song.elrc")
			writeFile(t, p, "[by:canticle]\n")
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
		})
		if filepath.Base(got) != "song.elrc" {
			t.Fatalf("ELRCCandidate = %q, want the unreadable exact name", got)
		}
	})
	t.Run("fifo", func(t *testing.T) {
		dir := t.TempDir()
		got := previewELRC(t, dir, func(dir string) {
			if err := mkfifo(filepath.Join(dir, "song.elrc")); err != nil {
				t.Skipf("mkfifo unsupported: %v", err)
			}
		})
		// A FIFO is not a regular file, so the exact-name rule reports no
		// candidate; reaching this line at all proves nothing opened it.
		if got != "" {
			t.Fatalf("ELRCCandidate = %q, want none for a FIFO", got)
		}
	})
}

func TestPreviewSourceRelativeSourcePathProbesNothing(t *testing.T) {
	sqlDB := openTestDB(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "song.lrc"), "[00:01.00]hi\n")
	t.Chdir(dir)
	id := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "T", status: "done"})
	for _, p := range []string{"song.flac", "  ", " song.flac"} {
		setSourcePath(t, sqlDB, id, p)
		got, err := reports.New(sqlDB).PreviewSource(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.LRCPath != "" || got.ELRCCandidate != "" {
			t.Fatalf("source_path %q: got %+v, want no sidecars", p, got)
		}
	}
}

func TestPreviewSourceNotFound(t *testing.T) {
	_, err := reports.New(openTestDB(t)).PreviewSource(context.Background(), 4242)
	if !errors.Is(err, reports.ErrPreviewNotFound) {
		t.Fatalf("err = %v, want ErrPreviewNotFound", err)
	}
}

func TestLibraryRootsLive(t *testing.T) {
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	ctx := context.Background()
	got, err := repo.LibraryRoots(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: got %v, %v", got, err)
	}
	for _, p := range []string{"/music/b", "/music/a"} {
		if _, err := sqlDB.ExecContext(ctx, "INSERT INTO libraries (path, name) VALUES (?, ?)", p, p); err != nil {
			t.Fatal(err)
		}
	}
	got, err = repo.LibraryRoots(ctx)
	if err != nil || !reflect.DeepEqual(got, []string{"/music/b", "/music/a"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := sqlDB.ExecContext(ctx, "DELETE FROM libraries WHERE path = '/music/a'"); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.LibraryRoots(ctx)
	if !reflect.DeepEqual(got, []string{"/music/b"}) {
		t.Fatalf("after delete got %v", got)
	}
}

func TestPreviewAudioPath(t *testing.T) {
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	ctx := context.Background()
	id := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "T", status: "done"})
	if got, err := repo.PreviewAudioPath(ctx, id); err != nil || got != "" {
		t.Fatalf("blank: got %q, %v", got, err)
	}
	setSourcePath(t, sqlDB, id, "  /music/a/song.flac \n")
	if got, err := repo.PreviewAudioPath(ctx, id); err != nil || got != "/music/a/song.flac" {
		t.Fatalf("got %q, %v, want the trimmed path", got, err)
	}
	if _, err := repo.PreviewAudioPath(ctx, id+999); !errors.Is(err, reports.ErrPreviewNotFound) {
		t.Fatalf("missing row: err = %v, want ErrPreviewNotFound", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PreviewAudioPath(ctx, id); err == nil || errors.Is(err, reports.ErrPreviewNotFound) {
		t.Fatalf("closed db: err = %v, want a wrapped query error", err)
	}
}
