package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
)

// readCounter is the metadata-reader seam: the scanner consults ShouldSkip
// exactly once per file it is about to tag-read, so its call count is the
// number of audio reads a scan cost.
type readCounter struct{ reads int }

func (c *readCounter) ShouldSkip(context.Context, string, int64, int64) (bool, error) {
	c.reads++
	return false, nil
}

func (c *readCounter) RecordFailure(context.Context, string, int64, int64, error) error {
	return nil
}

// A settled detector-written instrumental marker on an already-indexed 'done'
// row must cost no audio read on an ordinary scan, and must stay done (#1106).
// The scanner used to report such a marker as reopenable after a detector
// version bump, but the upsert preserves a done row's status and the queue never
// reopens a done row, so the reopen never happened and every scan re-read the
// audio. Real scan.Repo over real SQLite, per the repo convention.
func TestScan_SettledDetectorMarkerCostsNoAudioRead(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := scan.New(sqlDB)
	lib, err := library.New(sqlDB).Add(ctx, t.TempDir(), "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}

	dir := lib.Path
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 44100*30,
		map[string]string{"ARTIST": "Some Artist", "TITLE": "Some Title"}); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	marker := "[source:" + lyrics.SourceDetector + "]\n[dv:old-model]\n" + lyrics.InstrumentalMarker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "song.txt"), []byte(marker), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	counter := &readCounter{}
	sc := scanner.NewScanner(scanner.WithIndexStore(repo), scanner.WithMetadataFailureStore(counter))
	opts := scanner.ScanOptions{MaxDepth: 1}
	scanOnce := func(o scanner.ScanOptions) []models.ScanResult {
		t.Helper()
		res, err := sc.ScanLibrary(ctx, dir, o)
		if err != nil {
			t.Fatalf("ScanLibrary: %v", err)
		}
		return res
	}

	// First scan: the path is not indexed, so the marker is read once and
	// emitted. This is also the positive control that the counter sees reads.
	first := scanOnce(opts)
	if len(first) != 1 || counter.reads != 1 {
		t.Fatalf("first scan: results=%d reads=%d; want 1 result and 1 read", len(first), counter.reads)
	}
	// The worker settles the row; model that as the state the bug needed.
	first[0].LibraryID = lib.ID
	first[0].Status = scan.StatusDone
	if err := repo.Upsert(ctx, lib.ID, first, scan.UpsertOptions{}); err != nil {
		t.Fatalf("index done row: %v", err)
	}

	counter.reads = 0
	if res := scanOnce(opts); len(res) != 0 {
		t.Errorf("ordinary scan over a settled marker emitted %d results; want 0", len(res))
	}
	if counter.reads != 0 {
		t.Errorf("ordinary scan read the audio %d time(s); want 0 for a settled, indexed marker", counter.reads)
	}
	rows, err := repo.ListByLibrary(ctx, lib.ID)
	if err != nil || len(rows) != 1 || rows[0].Status != scan.StatusDone {
		t.Fatalf("row after scan = %+v, err=%v; want one done row", rows, err)
	}

	// --upgrade is still the reopen path: it emits the marker again.
	if res := scanOnce(scanner.ScanOptions{MaxDepth: 1, Upgrade: true}); len(res) != 1 {
		t.Errorf("--upgrade scan emitted %d results; want 1 (reopen preserved)", len(res))
	}
}
