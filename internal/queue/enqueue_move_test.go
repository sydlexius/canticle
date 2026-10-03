package queue

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

const (
	moveOld = "/lib/album/01 song.mp3"
	moveNew = "/lib/album/01 song.flac"
)

// fakeStat is the filesystem the gone-source check sees: a path maps to its
// stat error, and a path absent from the map is a present file.
func fakeStat(errs map[string]error) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) { return nil, errs[p] }
}

// seedMoveRow inserts one work_queue row at moveOld with a full settle record,
// then applies extra (a SET clause) to shape the case under test.
func seedMoveRow(t *testing.T, sqlDB *sql.DB, extra string) int64 {
	t.Helper()
	var id int64
	if err := sqlDB.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, outdir, filename,
             source_path, output_paths, status, attempts, outcome_type, timing_outcome, sync_tier, provider_lane, completed_at)
         VALUES ('Artist', 'Song', 'artist', 'song', '/lib/album', '01 song.lrc', ?,
             '[{"outdir":"/lib/album","filename":"01 song.lrc"}]', 'done', 2, 'synced', 'ok', 'line', 'musixmatch',
             '2026-01-10T00:00:00Z') RETURNING id`, moveOld).Scan(&id); err != nil {
		t.Fatalf("seed work_queue: %v", err)
	}
	if extra != "" {
		if _, err := sqlDB.Exec(`UPDATE work_queue SET `+extra+` WHERE id = ?`, id); err != nil { //nolint:gosec // reason: G202: extra is a test-authored literal
			t.Fatalf("shape row: %v", err)
		}
	}
	return id
}

// moveRow renders the columns a move may or may not touch.
func moveRow(t *testing.T, sqlDB *sql.DB, id int64) (row string) {
	t.Helper()
	if err := sqlDB.QueryRow(`SELECT source_path || '|' || status || '|' || attempts || '|' || COALESCE(outcome_type, '') || '|' ||
             COALESCE(timing_outcome, '') || '|' || COALESCE(sync_tier, '') || '|' || COALESCE(provider_lane, '') || '|' ||
             COALESCE(completed_at, '') || '|' || COALESCE(word_timing_state, '') || '|' || upgrade_queued || '|' ||
             outdir || '/' || filename || '|' || output_paths
         FROM work_queue WHERE id = ?`, id).Scan(&row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	return row
}

const moveTail = `|/lib/album/01 song.lrc|[{"outdir":"/lib/album","filename":"01 song.lrc"}]`

// TestEnqueueMovesRowToSameStemReplacement pins the #1262 repair and each guard
// around it: a collision moves the row only when its file is definitely gone,
// the incoming same-stem file is present, and the row is not in flight.
func TestEnqueueMovesRowToSameStemReplacement(t *testing.T) {
	gone := map[string]error{moveOld: fs.ErrNotExist}
	settled := "|done|2|synced|ok|line|musixmatch|2026-01-10T00:00:00Z|"
	tests := []struct {
		name     string
		extra    string
		stat     map[string]error
		incoming string
		want     string
	}{
		{"swap moves a done row and keeps its telemetry", "", gone, moveNew,
			moveNew + settled + "|0" + moveTail},
		{"old file still present: no move", "", nil, moveNew,
			moveOld + settled + "|0" + moveTail},
		{"stat error on the old file reads as present: no move", "",
			map[string]error{moveOld: errors.New("permission denied")}, moveNew,
			moveOld + settled + "|0" + moveTail},
		{"incoming file not present: no move", "",
			map[string]error{moveOld: fs.ErrNotExist, moveNew: fs.ErrNotExist}, moveNew,
			moveOld + settled + "|0" + moveTail},
		{"processing row is never moved", "status = 'processing'", gone, moveNew,
			// The upsert itself clears completed_at on an in-flight row, as before.
			moveOld + "|processing|2|synced|ok|line|musixmatch|||0" + moveTail},
		{"different stem is left as today", "", gone, "/lib/album/1-01 song.flac",
			moveOld + settled + "|0" + moveTail},
		{"different directory is left as today", "", gone, "/lib/other/01 song.flac",
			moveOld + settled + "|0" + moveTail},
		{"unavailable row moves and stays retired", "status = 'unavailable', last_error = 'miss limit reached'", gone, moveNew,
			moveNew + "|unavailable|2|synced|ok|line|musixmatch|2026-01-10T00:00:00Z||0" + moveTail},
		{"upgrade-armed row moves and stays armed", "status = 'pending', upgrade_queued = 1", gone, moveNew,
			moveNew + "|pending|2|synced|ok|line|musixmatch|2026-01-10T00:00:00Z||1" + moveTail},
		{"word-recheck row moves and stays in recheck mode", "status = 'deferred', word_timing_state = 'queued'", gone, moveNew,
			moveNew + "|deferred|2|synced|ok|line|musixmatch|2026-01-10T00:00:00Z|queued|0" + moveTail},
		{"row prune retired is reopened at the new path", "last_error = '" + UnresolvableGoneError + "'", gone, moveNew,
			moveNew + "|pending|0|||line|musixmatch|||0" + moveTail},
		{"hand-edited retired row moves but is not reopened",
			"last_error = '" + UnresolvableGoneError + "', lyric_edited_at = '2026-02-01T00:00:00Z'", gone, moveNew,
			moveNew + settled + "|0" + moveTail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sqlDB := openQueueTestDB(t)
			q := NewDBQueue(sqlDB)
			q.stat = fakeStat(tc.stat)
			id := seedMoveRow(t, sqlDB, tc.extra)
			// An unmarked caller (not FromScan), so a recheck row is not reopened
			// by the scan-origin rule and the move is what is under test.
			item, err := q.Enqueue(ctx, models.Inputs{
				Track:      models.Track{ArtistName: "Artist", TrackName: "Song"},
				Outdir:     "/lib/album",
				Filename:   "01 song.lrc",
				SourcePath: tc.incoming,
			}, PriorityScan)
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if item.ID != id {
				t.Fatalf("Enqueue returned row %d, want the colliding row %d", item.ID, id)
			}
			if got := moveRow(t, sqlDB, id); got != tc.want {
				t.Errorf("row after collision:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// seedSwap is seedMoveRow plus the vanished file's scan_result, linked to the
// row by scan_result_id alone; then setup runs (to index the incoming file).
func seedSwap(t *testing.T, sqlDB *sql.DB, extra, setup string) int64 {
	t.Helper()
	id := seedMoveRow(t, sqlDB, extra)
	if _, err := sqlDB.Exec(`UPDATE work_queue SET scan_result_id = ? WHERE id = ?;`+setup, //nolint:gosec // reason: G202: setup is a test-authored literal
		insertScanResult(t, sqlDB, moveOld), id); err != nil {
		t.Fatalf("seed swap: %v", err)
	}
	return id
}

const indexNew = `INSERT INTO scan_results (library_id, file_path, status)
         SELECT library_id, '` + moveNew + `', 'processing' FROM scan_results;`

// swapState renders "source|status|linked scan_result|junction|scan_results".
func swapState(t *testing.T, sqlDB *sql.DB, id int64) (got string) {
	t.Helper()
	if err := sqlDB.QueryRow(`SELECT replace(wq.source_path || '|' || wq.status || '|' ||
             COALESCE((SELECT file_path FROM scan_results WHERE id = wq.scan_result_id), '-') || '|' ||
             COALESCE((SELECT group_concat(sr.file_path) FROM work_queue_scan_results j
                 JOIN scan_results sr ON sr.id = j.scan_result_id WHERE j.work_queue_id = wq.id), '-') || '|' ||
             COALESCE((SELECT group_concat(file_path || ':' || status) FROM (SELECT * FROM scan_results ORDER BY id)), '-'),
             '/lib/album/01 song', '')
         FROM work_queue wq WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("read swap state: %v", err)
	}
	return got
}

// TestGoneSourceMoveScanResultsAndRaces pins the guards against a writer running
// between plan and write: race runs inside the stat seam, after the plan's read.
func TestGoneSourceMoveScanResultsAndRaces(t *testing.T) {
	const otherInFlight = `INSERT INTO work_queue (artist, title, artist_key, title_key, outdir, filename, status)
             VALUES ('B', 'B', 'b', 'b', '/x', 'b.lrc', 'processing');
         INSERT INTO work_queue_scan_results SELECT wq.id, sr.id FROM work_queue wq, scan_results sr
             WHERE wq.artist_key = 'b' AND sr.file_path LIKE '%.mp3'`
	tests := []struct{ name, extra, setup, race, want string }{
		{name: "row re-pointed between plan and write: nothing happens",
			race: `UPDATE work_queue SET source_path = '/lib/album/01 song.ogg'`,
			want: ".ogg|done|.mp3|-|.mp3:processing,.flac:processing"},
		{name: "row claimed between plan and write: nothing happens",
			race: `UPDATE work_queue SET status = 'processing'`,
			want: ".mp3|processing|.mp3|-|.mp3:processing,.flac:processing"},
		{name: "vanished scan_result in flight under another row is kept", setup: otherInFlight,
			want: ".flac|done|.flac|.flac|.mp3:processing,.flac:done"},
		{name: "repoint links the junction; an unsettled row leaves the scan_result unsettled",
			extra: "status = 'pending', upgrade_queued = 1", want: ".flac|pending|.flac|.flac|.flac:processing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sqlDB := openQueueTestDB(t)
			q := NewDBQueue(sqlDB)
			id := seedSwap(t, sqlDB, tc.extra, indexNew+tc.setup)
			q.stat = func(p string) (fs.FileInfo, error) {
				if p != moveOld {
					return nil, nil
				}
				if _, err := sqlDB.Exec(tc.race); err != nil {
					t.Errorf("competing write: %v", err)
				}
				return nil, fs.ErrNotExist
			}
			in := models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Song"}, SourcePath: moveNew}
			if moved, err := q.RepointGoneSource(ctx, in); err != nil || moved != (tc.race == "") {
				t.Errorf("RepointGoneSource = (%v, %v), want moved=%v", moved, err, tc.race == "")
			}
			if got := swapState(t, sqlDB, id); got != tc.want {
				t.Errorf("state after the move:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// A webhook for the replacement arrives before any scan has indexed it (#1262 F3).
func TestPathOnlyMoveKeepsLinkUntilScanRelinks(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	stats, errs := 0, map[string]error{moveOld: fs.ErrNotExist}
	q.stat = func(p string) (fs.FileInfo, error) {
		stats++
		return nil, errs[p]
	}
	id := seedSwap(t, sqlDB, "", "")
	in := models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Song"}, SourcePath: moveNew}
	if _, err := q.Enqueue(ctx, in, PriorityWebhook); err != nil {
		t.Fatalf("webhook Enqueue: %v", err)
	}
	if got, want := swapState(t, sqlDB, id), ".flac|done|.mp3|-|.mp3:processing"; got != want {
		t.Fatalf("after the webhook: got %s, want %s (vanished scan_result kept and linked)", got, want)
	}
	// No relink with nothing to link to yet (i == 0), nor, once the replacement
	// is indexed, while the linked file is not definitely gone.
	for i, setup := range []string{"", indexNew} {
		if _, err := sqlDB.Exec(setup); err != nil {
			t.Fatalf("index the replacement: %v", err)
		}
		if moved, err := q.RepointGoneSource(ctx, in); err != nil || moved {
			t.Fatalf("early repoint %d = (%v, %v), want no relink", i, moved, err)
		}
		errs[moveOld] = errors.New("unavailable mount")
	}
	errs[moveOld] = fs.ErrNotExist
	for _, step := range [][2]string{
		{"", ".flac|done|.flac|.flac|.flac:done"}, // the stale link is swapped
		{`UPDATE work_queue SET scan_result_id = NULL, last_error = '` + UnresolvableGoneError + `'; DELETE FROM work_queue_scan_results;
          UPDATE scan_results SET status = 'pending'`, ".flac|done|.flac|.flac|.flac:pending"}, // no link: linked, never reopened
	} {
		if _, err := sqlDB.Exec(step[0]); err != nil {
			t.Fatalf("unlink: %v", err)
		}
		if moved, err := q.RepointGoneSource(ctx, in); err != nil || !moved {
			t.Fatalf("scan repoint = (%v, %v), want a relink", moved, err)
		}
		if got := swapState(t, sqlDB, id); got != step[1] {
			t.Fatalf("after the scan: got %s, want %s", got, step[1])
		}
	}
	stats = 0
	if moved, err := q.RepointGoneSource(ctx, in); err != nil || moved || stats != 0 {
		t.Errorf("repoint of a correctly linked row = (moved %v, %v, %d stats), want a no-op with no stat", moved, err, stats)
	}
}
