package prune

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

// addLibraryB registers a second library root beside root, populated so it is
// online, and returns its path.
func addLibraryB(t *testing.T, ctx context.Context, sqlDB *sql.DB, root string) string {
	t.Helper()
	b := filepath.Join(filepath.Dir(root), "musicB")
	if err := os.MkdirAll(filepath.Join(b, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := library.New(sqlDB).Add(ctx, b, "libB", models.LibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// linkScanResult adds a scan_results row for file in library libID and links it
// to the work item through the junction, as the work_queue dedupe does.
func linkScanResult(t *testing.T, ctx context.Context, sqlDB *sql.DB, libID, wqID int64, file string) {
	t.Helper()
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename) VALUES (?, ?, 'Artist', 'second', 'done', ?, ?)`,
		libID, file, filepath.Dir(file), filepath.Base(file))
	if err != nil {
		t.Fatal(err)
	}
	srID, _ := res.LastInsertId()
	execWQ(t, ctx, sqlDB, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wqID, srID)
}

// multiRow seeds an eligible row whose own copy exists and whose second entry is
// elsewhere (second), with the same filename.
func multiRow(t *testing.T, ctx context.Context, sqlDB *sql.DB, libID int64, root, second string) (int64, string) {
	t.Helper()
	src := filepath.Join(root, "amb", "New", "01.flac")
	id := seedRowWithOutputPaths(t, ctx, sqlDB, libID, src, "amb", []models.OutputPath{
		{Outdir: filepath.Dir(src), Filename: "01.flac"},
		{Outdir: second, Filename: "01.flac"},
	})
	execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = 'failed' WHERE id = ?`, id)
	return id, rawOutputPaths(t, ctx, sqlDB, id)
}

// EntryProvablyStale is the one predicate the worker's settle and the attended
// drop share (#1430). Each case changes exactly one of its three conditions.
func TestEntryProvablyStale(t *testing.T) {
	online := func(string) bool { return true }
	offline := func(string) bool { return false }
	cases := []struct {
		name  string
		setup func(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64, root string, libID int64) models.OutputPath
		probe func(string) bool
		want  bool
	}{
		{name: "unlinked-entry-under-online-root", probe: online, want: true,
			setup: func(_ *testing.T, _ context.Context, _ *sql.DB, _ int64, root string, _ int64) models.OutputPath {
				return models.OutputPath{Outdir: filepath.Join(root, "amb", "Old"), Filename: "01.flac"}
			}},
		{name: "root-offline", probe: offline, want: false,
			setup: func(_ *testing.T, _ context.Context, _ *sql.DB, _ int64, root string, _ int64) models.OutputPath {
				return models.OutputPath{Outdir: filepath.Join(root, "amb", "Old"), Filename: "01.flac"}
			}},
		{name: "under-no-configured-root", probe: online, want: false,
			setup: func(t *testing.T, _ context.Context, _ *sql.DB, _ int64, _ string, _ int64) models.OutputPath {
				return models.OutputPath{Outdir: filepath.Join(t.TempDir(), "elsewhere"), Filename: "01.flac"}
			}},
		{name: "no-link-rows", probe: online, want: false,
			setup: func(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64, root string, libID int64) models.OutputPath {
				execWQ(t, ctx, sqlDB, `DELETE FROM work_queue_scan_results WHERE work_queue_id = ?`, id)
				return models.OutputPath{Outdir: filepath.Join(root, "amb", "Old"), Filename: "01.flac"}
			}},
		{name: "linked-file-inside-entry", probe: online, want: false,
			setup: func(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64, root string, libID int64) models.OutputPath {
				second := filepath.Join(root, "amb", "Second")
				linkScanResult(t, ctx, sqlDB, libID, id, filepath.Join(second, "01.flac"))
				return models.OutputPath{Outdir: second, Filename: "01.flac"}
			}},
		{name: "own-source-inside-entry", probe: online, want: false,
			setup: func(t *testing.T, ctx context.Context, sqlDB *sql.DB, id int64, root string, libID int64) models.OutputPath {
				// Links exist but only to a file elsewhere: the source clause alone decides.
				execWQ(t, ctx, sqlDB, `DELETE FROM work_queue_scan_results WHERE work_queue_id = ?`, id)
				linkScanResult(t, ctx, sqlDB, libID, id, filepath.Join(root, "amb", "Elsewhere", "02.flac"))
				return models.OutputPath{Outdir: filepath.Join(root, "amb", "New"), Filename: "01.flac"}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			id, _ := multiRow(t, ctx, sqlDB, libID, root, filepath.Join(root, "amb", "Old"))
			src := filepath.Join(root, "amb", "New", "01.flac")
			e := tc.setup(t, ctx, sqlDB, id, root, libID)
			got, err := New(sqlDB).EntryProvablyStale(ctx, id, src, e, tc.probe)
			if err != nil || got != tc.want {
				t.Errorf("EntryProvablyStale = %v err=%v, want %v", got, err, tc.want)
			}
		})
	}
}
