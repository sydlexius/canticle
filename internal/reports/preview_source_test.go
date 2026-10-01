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
		ID: id, Artist: "Ar", Title: "Ti", Album: "Al", Status: "done", SyncTier: "word",
		AudioPath: audio,
		LRCPath:   filepath.Join(dir, "song.lrc"),
		ELRCPath:  filepath.Join(dir, "song.elrc"),
	}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestPreviewSourceForeignCompanionNotReported(t *testing.T) {
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
	if got.LRCPath == "" || got.ELRCPath != "" {
		t.Fatalf("lrc=%q elrc=%q: want lrc set, foreign elrc empty", got.LRCPath, got.ELRCPath)
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
	if got.AudioPath != audio || got.LRCPath != "" || got.ELRCPath != "" {
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
	if got.AudioPath != "" || got.LRCPath != "" || got.ELRCPath != "" || got.Status != "pending" {
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
		if got.LRCPath != "" || got.ELRCPath != "" {
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
