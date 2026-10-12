package commands

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
)

// The three fixtures of #1586, by the lyric file name the scan records.
const (
	reopenTxt    = "alpha.lrc" // a plain unsynced .txt, last modified two days ago
	reopenMarker = "beta.lrc"  // a bare instrumental-marker .txt, modified now
	reopenLRC    = "gamma.lrc" // a synced .lrc
)

// reopenLibrary makes a database and a library of three tagged audio files;
// withLyrics also writes the lyric file beside each.
func reopenLibrary(t *testing.T, withLyrics bool) (*sql.DB, string) {
	t.Helper()
	sqlDB := openBackfillDB(t)
	root := t.TempDir()
	if _, err := library.New(sqlDB).Add(context.Background(), root, "Music", models.LibrarySettings{}); err != nil {
		t.Fatal(err)
	}
	for stem, title := range map[string]string{"alpha": "Moss Gate", "beta": "Pale Orchard", "gamma": "Soft Ledger"} {
		if err := testutil.WriteAudioFile(root, stem+".mp3", "Quill Ember", title, "Album X", ""); err != nil {
			t.Fatal(err)
		}
	}
	if withLyrics {
		writeReopenLyrics(t, root)
	}
	return sqlDB, root
}

func writeReopenLyrics(t *testing.T, root string) {
	t.Helper()
	for name, body := range map[string]string{
		"alpha.txt": "first invented line\nsecond invented line\n",
		"beta.txt":  lyrics.InstrumentalMarker + "\n",
		"gamma.lrc": "[00:01.00]invented synced line\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "alpha.txt"), old, old); err != nil {
		t.Fatal(err)
	}
}

var enqueuedLogRE = regexp.MustCompile(`scheduled scan complete.* enqueued=(\d+)`)

// reopenScan runs one scan through the production scheduler and returns the
// enqueued count its completion log line reported.
func reopenScan(t *testing.T, sqlDB *sql.DB, o scanner.ScanOptions) int {
	t.Helper()
	logs := captureSlog(t)
	o.MaxDepth = 1
	s := scheduler(sqlDB, o, nil, false, nil, nil, "", 0, nil)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	m := enqueuedLogRE.FindStringSubmatch(logs.String())
	if m == nil {
		t.Fatalf("no scan-complete log line in:\n%s", logs.String())
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

type reopenRow struct {
	id                  int64
	scan, queue, manual string
	adopted             bool
	rows                int // queue rows linked to the file's scan row
}

func reopenRows(t *testing.T, sqlDB *sql.DB) map[string]reopenRow {
	t.Helper()
	rows, err := sqlDB.Query(`SELECT sr.filename, sr.status, COALESCE(wq.id, 0), COALESCE(wq.status, ''),
	         COALESCE(wq.manual_instrumental_at, ''), wq.adopted_at IS NOT NULL,
	         (SELECT COUNT(*) FROM work_queue_scan_results j WHERE j.scan_result_id = sr.id)
	     FROM scan_results sr
	     LEFT JOIN work_queue_scan_results l ON l.scan_result_id = sr.id
	     LEFT JOIN work_queue wq ON wq.id = l.work_queue_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // reason: test-only read
	out := map[string]reopenRow{}
	for rows.Next() {
		var name string
		var r reopenRow
		if err := rows.Scan(&name, &r.scan, &r.id, &r.queue, &r.manual, &r.adopted, &r.rows); err != nil {
			t.Fatal(err)
		}
		out[name] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// wantReopened asserts which files a scan re-queued. A re-queued file has a
// pending queue row with the adoption mark spent and its scan row in flight;
// every other file keeps its done row, and its scan row agrees with it.
func wantReopened(t *testing.T, sqlDB *sql.DB, reopened ...string) {
	t.Helper()
	got := reopenRows(t, sqlDB)
	if len(got) != 3 {
		t.Fatalf("scan rows = %d, want 3", len(got))
	}
	want := map[string]bool{}
	for _, name := range reopened {
		want[name] = true
	}
	for name, r := range got {
		if r.rows != 1 {
			t.Errorf("%s: %d queue rows linked, want 1", name, r.rows)
		}
		if want[name] {
			if r.queue != "pending" || r.scan != "processing" || r.adopted {
				t.Errorf("%s: queue=%s scan=%s adopted=%v, want re-queued (pending/processing, mark spent)", name, r.queue, r.scan, r.adopted)
			}
			continue
		}
		if r.queue != "done" || r.scan != "done" {
			t.Errorf("%s: queue=%s scan=%s, want both done (not re-queued)", name, r.queue, r.scan)
		}
	}
}

// After an ordinary scan adopted them, each reopen flag re-queues exactly the
// files it selects (#1586), and the log's enqueued count is that number.
func TestScanReopenFlagRequeuesAdoptedFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		opts scanner.ScanOptions
		want []string
	}{
		"upgrade":         {scanner.ScanOptions{Upgrade: true}, []string{reopenTxt, reopenMarker}},
		"update":          {scanner.ScanOptions{Update: true}, []string{reopenTxt, reopenMarker, reopenLRC}},
		"unsynced-before": {scanner.ScanOptions{Upgrade: true, UnsyncedBefore: time.Now().Add(-24 * time.Hour)}, []string{reopenTxt}},
		"bare-markers":    {scanner.ScanOptions{Upgrade: true, BareMarkersOnly: true}, []string{reopenMarker}},
	} {
		t.Run(name, func(t *testing.T) {
			sqlDB, _ := reopenLibrary(t, true)
			if n := reopenScan(t, sqlDB, scanner.ScanOptions{}); n != 0 {
				t.Fatalf("ordinary scan enqueued = %d, want 0 (every file is settled)", n)
			}
			wantReopened(t, sqlDB)
			for file, r := range reopenRows(t, sqlDB) {
				if !r.adopted {
					t.Fatalf("%s: not marked adopted after the ordinary scan", file)
				}
			}
			if n := reopenScan(t, sqlDB, tc.opts); n != len(tc.want) {
				t.Errorf("enqueued = %d, want %d", n, len(tc.want))
			}
			wantReopened(t, sqlDB, tc.want...)
			// An ordinary scan must not adopt a re-queued file back to done.
			if n := reopenScan(t, sqlDB, scanner.ScanOptions{}); n != 0 {
				t.Errorf("following ordinary scan enqueued = %d, want 0", n)
			}
			wantReopened(t, sqlDB, tc.want...)
		})
	}
}

// A done row the worker completed is not reopened by any flag, and its scan
// row is not left at processing against it.
func TestScanReopenFlagKeepsWorkerCompletedRows(t *testing.T) {
	ctx := context.Background()
	sqlDB, root := reopenLibrary(t, false)
	if n := reopenScan(t, sqlDB, scanner.ScanOptions{}); n != 3 {
		t.Fatalf("first scan enqueued = %d, want 3", n)
	}
	q := queue.NewDBQueue(sqlDB)
	for range 3 {
		item, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Complete(ctx, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	writeReopenLyrics(t, root)
	for name, o := range map[string]scanner.ScanOptions{"upgrade": {Upgrade: true}, "update": {Update: true}} {
		if n := reopenScan(t, sqlDB, o); n != 0 {
			t.Errorf("%s: enqueued = %d, want 0", name, n)
		}
		wantReopened(t, sqlDB)
	}
}

// A manual instrumental mark and a hand edit are never reopened.
func TestScanReopenFlagKeepsHandProtectedRows(t *testing.T) {
	ctx := context.Background()
	sqlDB, _ := reopenLibrary(t, true)
	reopenScan(t, sqlDB, scanner.ScanOptions{})
	before := reopenRows(t, sqlDB)
	q := queue.NewDBQueue(sqlDB)
	if changed, err := q.MarkManualInstrumental(ctx, before[reopenMarker].id); err != nil || !changed {
		t.Fatalf("mark = %v, %v", changed, err)
	}
	if err := q.SetLyricEdit(ctx, before[reopenLRC].id, 250); err != nil {
		t.Fatal(err)
	}
	if n := reopenScan(t, sqlDB, scanner.ScanOptions{Update: true}); n != 1 {
		t.Errorf("enqueued = %d, want 1 (the unprotected .txt only)", n)
	}
	wantReopened(t, sqlDB, reopenTxt)
	if after := reopenRows(t, sqlDB); after[reopenMarker].manual == "" || !after[reopenMarker].adopted || !after[reopenLRC].adopted {
		t.Errorf("protected rows were written: %+v %+v", after[reopenMarker], after[reopenLRC])
	}
}

// A file first seen by an --upgrade scan is queued pending, never adopted; the
// settled .lrc the flag does not select is adopted in the same pass.
func TestFirstSightUpgradeScanQueuesSettledTxtFiles(t *testing.T) {
	sqlDB, _ := reopenLibrary(t, true)
	if n := reopenScan(t, sqlDB, scanner.ScanOptions{Upgrade: true}); n != 2 {
		t.Errorf("enqueued = %d, want 2", n)
	}
	wantReopened(t, sqlDB, reopenTxt, reopenMarker)
	if r := reopenRows(t, sqlDB)[reopenLRC]; !r.adopted {
		t.Errorf("settled .lrc row = %+v, want it adopted", r)
	}
}

// An ordinary scan reopens nothing, even for an adopted file it emits as
// pending (its lyric file is gone and its scan row was reset to pending).
func TestOrdinaryScanNeverReopensAnAdoptedRow(t *testing.T) {
	sqlDB, root := reopenLibrary(t, true)
	reopenScan(t, sqlDB, scanner.ScanOptions{})
	if err := os.Remove(filepath.Join(root, "alpha.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE scan_results SET status = 'pending' WHERE filename = ?`, reopenTxt); err != nil {
		t.Fatal(err)
	}
	if n := reopenScan(t, sqlDB, scanner.ScanOptions{}); n != 0 {
		t.Errorf("enqueued = %d, want 0", n)
	}
	wantReopened(t, sqlDB)
}

// A settled .lrc whose scan row is pending for a reason other than this scan's
// flag is not reopened by --upgrade, which never selects a settled .lrc.
func TestUpgradeScanKeepsAPendingRowItDidNotSelect(t *testing.T) {
	sqlDB, _ := reopenLibrary(t, true)
	reopenScan(t, sqlDB, scanner.ScanOptions{})
	if _, err := sqlDB.Exec(`UPDATE scan_results SET status = 'pending' WHERE filename = ?`, reopenLRC); err != nil {
		t.Fatal(err)
	}
	if n := reopenScan(t, sqlDB, scanner.ScanOptions{Upgrade: true}); n != 2 {
		t.Errorf("enqueued = %d, want 2 (the two .txt files)", n)
	}
	wantReopened(t, sqlDB, reopenTxt, reopenMarker)
}

// A row retired as unavailable stays so under a reopen flag, and the scan rows
// the flag forced pending are settled against it, not left at processing.
func TestScanReopenFlagSettlesScanRowsOfUnavailableRows(t *testing.T) {
	ctx := context.Background()
	sqlDB, _ := reopenLibrary(t, false)
	reopenScan(t, sqlDB, scanner.ScanOptions{})
	q := queue.NewDBQueue(sqlDB)
	for range 3 {
		item, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.RetireMiss(ctx, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	reopenScan(t, sqlDB, scanner.ScanOptions{Update: true})
	for name, r := range reopenRows(t, sqlDB) {
		if r.queue != "unavailable" || r.scan != "done" {
			t.Errorf("%s: queue=%s scan=%s, want unavailable with a done scan row", name, r.queue, r.scan)
		}
	}
}

// Two files of one song share a queue row, which the count reports once.
func TestScanCountsAQueueRowOnceForTwoFilesOfOneSong(t *testing.T) {
	sqlDB, root := reopenLibrary(t, false)
	if err := testutil.WriteAudioFile(root, "delta.mp3", "Quill Ember", "Moss Gate", "Album Y", ""); err != nil {
		t.Fatal(err)
	}
	if n := reopenScan(t, sqlDB, scanner.ScanOptions{}); n != 3 {
		t.Errorf("enqueued = %d, want 3 (four files, three songs)", n)
	}
}
