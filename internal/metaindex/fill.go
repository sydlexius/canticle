package metaindex

import (
	"context"
	"database/sql"
	"log/slog"
)

// FillScanRecordingMBID copies a file's recording ID from the tag cache onto
// its scan_results row (#1520). It FILLS ONLY: the UPDATE matches rows whose
// recording_mbid is empty, so a non-empty value is never overwritten here (a
// scan rewrites it, and the scan upsert keeps a non-empty value over an empty
// one). A settled file is never opened by a scan, so without this a row stored
// before the Vorbis fix keeps an empty ID forever and `scan reconcile-paths`
// retains it as "identity absent".
//
// The scanner stores file_path as the walk path under the library root as
// configured, while the tag-cache key is the symlink-resolved path, so a
// symlinked root spells the same file two ways: the row matches under either
// walkPath or canonPath (equal for an ordinary root, so nothing double counts).
//
// A row for a file that is gone is never visited, because the caller walks
// files that exist. Best-effort: a failure is logged and the run continues,
// since audio_metadata is this command's product. A dry run only counts the
// rows it would fill and writes nothing. It returns the number of rows filled
// (on a dry run, that would be), never IDs.
//
// A log line carries a fixed reason and the driver error as "cause", never a
// path: the driver error does not echo the bound path arguments.
func FillScanRecordingMBID(ctx context.Context, sqlDB *sql.DB, walkPath, path, mbid string, apply bool) int {
	if mbid == "" {
		return 0
	}
	if !apply {
		// COUNT(*) over the same predicate as the UPDATE below, so the dry run
		// reports the number of rows the real run fills (two rows may share a path).
		var n int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM scan_results WHERE (file_path = ? OR file_path = ?) AND recording_mbid = ''`, walkPath, path).Scan(&n); err != nil {
			slog.Warn("index-metadata: could not check the scan row for a recording ID", "reason", "scan row check failed", "cause", err)
			return 0
		}
		return n
	}
	res, err := sqlDB.ExecContext(ctx,
		`UPDATE scan_results SET recording_mbid = ? WHERE (file_path = ? OR file_path = ?) AND recording_mbid = ''`, mbid, walkPath, path)
	if err != nil {
		slog.Warn("index-metadata: could not fill the scan row's recording ID", "reason", "scan row fill failed", "cause", err)
		return 0
	}
	if n, rerr := res.RowsAffected(); rerr == nil {
		return int(n)
	}
	return 0
}
