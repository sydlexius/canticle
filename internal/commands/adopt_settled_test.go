package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
)

// The post-scan hook adopts a settled file, writes its backup beside the
// database, and logs COUNTS ONLY (no path, artist or title in any log line).
func TestAdoptSettledAfterScanLogsCountsOnlyAndBacksUpBesideDB(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := dbtest.OpenAt(ctx, t, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	root := t.TempDir()
	lib, err := library.New(sqlDB).Add(ctx, root, "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	const artist, title, stem = "Zebra Quartz", "Mauve Lantern", "mauve-lantern-secret"
	audio := filepath.Join(root, stem+".mp3")
	for p, c := range map[string]string{audio: "a", filepath.Join(root, stem+".lrc"): "[00:01.00]alpha\n"} {
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := scan.New(sqlDB).Upsert(ctx, lib.ID, []models.ScanResult{{
		FilePath: audio, Track: models.Track{ArtistName: artist, TrackName: title}, Outdir: root, Filename: stem + ".lrc", Status: scan.StatusDone,
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	adoptSettledAfterScan(ctx, sqlDB, queue.NewDBQueue(sqlDB), lib, root, "scheduler")

	logs := buf.String()
	for _, want := range []string{"inserted=1", "already_linked=0", "unexpected=0"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q: %q", want, logs)
		}
	}
	for _, private := range []string{artist, title, stem, root} {
		if strings.Contains(logs, private) {
			t.Errorf("log leaked %q: %s", private, logs)
		}
	}
	backup, err := os.ReadFile(filepath.Join(filepath.Dir(dbPath), adoptSettledBackupName))
	if err != nil || !strings.Contains(string(backup), `"work_queue_id"`) {
		t.Errorf("backup beside the database = %q, %v; want one JSONL record", backup, err)
	}
}

// The scheduler's post-scan callback adopts a settled file: only its call to
// adoptSettledAfterScan can give this file (lyric beside it, so never enqueued) a
// queue row. The call ORDER (after RepointSettled, before EnqueuePending) is not
// observable: the three touch disjoint rows.
func TestSchedulerAdoptsASettledFileAfterTheScan(t *testing.T) {
	ctx := context.Background()
	sqlDB := openBackfillDB(t)
	lib, err := library.New(sqlDB).Add(ctx, t.TempDir(), "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lib.Path, "01 song.lrc"), []byte("[00:01.00]la\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testutil.WriteFLACFileWithComments(lib.Path, "01 song.flac", 44100, 44100*30,
		map[string]string{"ARTIST": "Some Artist", "TITLE": "Some Title"}); err != nil {
		t.Fatal(err)
	}
	s := scheduler(sqlDB, scanner.ScanOptions{MaxDepth: 1}, nil, false, nil, nil, "", 0, nil)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	var status, outcome string
	var links int
	if err := sqlDB.QueryRowContext(ctx, `SELECT status, outcome_type, (SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = work_queue.id) FROM work_queue`).
		Scan(&status, &outcome, &links); err != nil {
		t.Fatalf("the settled file has no queue row after the scan: %v", err)
	}
	if status != "done" || outcome != "synced" || links != 1 {
		t.Errorf("row = %s/%s links=%d, want a linked done/synced row", status, outcome, links)
	}
}

// faultyHandle wraps a real file and injects a short write, a failed sync, or
// a failed rollback truncate.
type faultyHandle struct {
	*os.File
	short, failSync, failTrunc bool
}

func (h *faultyHandle) Write(p []byte) (int, error) {
	if h.short {
		return h.File.Write(p[:len(p)/2])
	}
	return h.File.Write(p)
}

func (h *faultyHandle) Sync() error {
	if h.failSync {
		h.failSync = false
		return errors.New("injected sync failure")
	}
	return h.File.Sync()
}

func (h *faultyHandle) Truncate(n int64) error {
	if h.failTrunc {
		return errors.New("injected truncate failure")
	}
	return h.File.Truncate(n)
}

// A short write or a failed sync rolls the file back to its prior length, so a
// later append yields only well-formed JSONL; a failed rollback is named.
func TestAppendLineOrRollbackLeavesNoPartialTail(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    faultyHandle
	}{{"short write", faultyHandle{}}, {"sync failure", faultyHandle{}}} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.OpenFile(filepath.Join(t.TempDir(), "b.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			tc.h.File = f
			first := []byte("{\"n\":1}\n")
			if err := appendLineOrRollback(&tc.h, first); err != nil {
				t.Fatal(err)
			}
			tc.h.short, tc.h.failSync = tc.name == "short write", tc.name == "sync failure"
			if err := appendLineOrRollback(&tc.h, []byte("{\"n\":2}\n")); err == nil {
				t.Fatal("injected fault produced no error")
			}
			if got, _ := os.ReadFile(f.Name()); !bytes.Equal(got, first) {
				t.Fatalf("file after a failed append = %q, want it unchanged", got)
			}
			tc.h.short = false
			if err := appendLineOrRollback(&tc.h, []byte("{\"n\":3}\n")); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(f.Name())
			for _, l := range strings.Split(strings.TrimSuffix(string(got), "\n"), "\n") {
				if !json.Valid([]byte(l)) {
					t.Errorf("malformed JSONL line %q", l)
				}
			}
		})
	}
	t.Run("rollback failure is reported and path-free", func(t *testing.T) {
		f, err := os.OpenFile(filepath.Join(t.TempDir(), "b.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		err = appendLineOrRollback(&faultyHandle{File: f, short: true, failTrunc: true}, []byte("{\"n\":1}\n"))
		if err == nil || !strings.Contains(err.Error(), "rollback failed") || strings.Contains(err.Error(), f.Name()) {
			t.Errorf("err = %v, want a path-free rollback-failed error", err)
		}
	})
}
