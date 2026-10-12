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

// A bare marker whose queue row is already done is emitted by the scanner under
// --upgrade --bare-markers, but the index upsert keeps the row done: a scan does
// not re-queue it (#618). Such markers are reached by the serve-mode upgrade
// sweep, which the docs say.
func TestScan_BareMarkerWithDoneRowStaysDone(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := scan.New(sqlDB)
	lib, err := library.New(sqlDB).Add(ctx, t.TempDir(), "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	if err := testutil.WriteFLACFileWithComments(lib.Path, "song.flac", 44100, 44100*30,
		map[string]string{"ARTIST": "Some Artist", "TITLE": "Some Title"}); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lib.Path, "song.txt"), []byte(lyrics.InstrumentalMarker+"\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	sc := scanner.NewScanner(scanner.WithIndexStore(repo))
	res, err := sc.ScanLibrary(ctx, lib.Path, scanner.ScanOptions{MaxDepth: 1})
	if err != nil || len(res) != 1 {
		t.Fatalf("first scan: results=%d err=%v; want 1", len(res), err)
	}
	res[0].LibraryID = lib.ID
	res[0].Status = scan.StatusDone
	if err := repo.Upsert(ctx, lib.ID, res, scan.UpsertOptions{}); err != nil {
		t.Fatalf("index done row: %v", err)
	}

	again, err := sc.ScanLibrary(ctx, lib.Path, scanner.ScanOptions{MaxDepth: 1, Upgrade: true, BareMarkersOnly: true})
	if err != nil || len(again) != 1 {
		t.Fatalf("bare-markers scan: results=%d err=%v; want the marker emitted", len(again), err)
	}
	again[0].LibraryID = lib.ID
	if err := repo.Upsert(ctx, lib.ID, again, scan.UpsertOptions{}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rows, err := repo.ListByLibrary(ctx, lib.ID)
	if err != nil || len(rows) != 1 || rows[0].Status != scan.StatusDone {
		t.Fatalf("rows = %+v, err=%v; want one row still done", rows, err)
	}
}
