package commands

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
)

// ProvenanceCmd hosts nested provenance subcommands.
type ProvenanceCmd struct {
	Backfill *ProvenanceBackfillCmd `arg:"subcommand:backfill" help:"inject provenance tags into existing .lrc files from the DB"`
}

// ProvenanceBackfillCmd injects provenance tags into existing .lrc files.
type ProvenanceBackfillCmd struct {
	Paths      []string `arg:"positional" help:"paths or directories to process (default: all known .lrc files from the DB)"`
	Yes        bool     `arg:"--yes" help:"apply the changes; without it, prints what would change and exits 0"`
	ConfigPath string   `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

func runProvenance(ctx context.Context, out io.Writer, args ProvenanceCmd) int {
	switch {
	case args.Backfill != nil:
		return runProvenanceBackfill(ctx, out, *args.Backfill)
	default:
		_, _ = fmt.Fprintln(out, "missing provenance subcommand")
		return 2
	}
}

// provenanceRecord holds the provenance data retrieved from the DB for one .lrc file.
type provenanceRecord struct {
	Source    string
	FetchedAt time.Time
	ISRC      string
	MBID      string
}

// provenancePlan describes what would happen to one .lrc file.
type provenancePlan struct {
	Path    string
	Tags    lyrics.ProvenanceTags
	Partial bool // true when some tags are missing because the DB lacks data
}

func runProvenanceBackfill(ctx context.Context, out io.Writer, args ProvenanceBackfillCmd) int {
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	var plans []provenancePlan
	var planSkipped int
	if len(args.Paths) == 0 {
		plans, planSkipped, err = backfillPlansFromDB(ctx, sqlDB)
	} else {
		plans, planSkipped, err = backfillPlansFromPaths(ctx, sqlDB, args.Paths)
	}
	if err != nil {
		slog.Error("backfill plan failed", "error", err)
		return 1
	}

	// Tally DB-coverage counts for the plan report.
	var seeded, partial int
	for _, p := range plans {
		if p.Partial {
			partial++
		} else {
			seeded++
		}
	}

	if !args.Yes {
		// MINOR-8: label the plan output unambiguously so users understand that
		// 'ready' is DB-coverage (not file state); already-tagged files appear as
		// 'unchanged' in the apply run.
		_, _ = fmt.Fprintf(out, "provenance backfill plan: %d files ready (all tags available in DB), %d partial (some DB tags missing), %d skipped (no DB row)\n",
			seeded, partial, planSkipped)
		_, _ = fmt.Fprintln(out, "Note: files already fully tagged appear as unchanged when --yes is applied.")
		_, _ = fmt.Fprintln(out, "pass --yes to apply")
		return 0
	}

	// Apply the plan.
	var applied, appliedPartial, unchanged, errCount int
	var failed []string // MINOR-9: track which files fail for stdout report
	for _, p := range plans {
		inj, _, ierr := lyrics.InjectProvenance(p.Path, p.Tags)
		if ierr != nil {
			slog.Warn("backfill inject failed", "path", p.Path, "error", ierr)
			errCount++
			failed = append(failed, p.Path)
			continue
		}
		if inj == 0 {
			unchanged++
		} else if p.Partial {
			appliedPartial++
		} else {
			applied++
		}
	}

	_, _ = fmt.Fprintf(out, "provenance backfill: applied %d, partial %d, unchanged %d, errors %d\n",
		applied, appliedPartial, unchanged, errCount)
	if errCount > 0 {
		for _, f := range failed {
			_, _ = fmt.Fprintf(out, "  failed: %s\n", f)
		}
		return 1
	}
	return 0
}

// wqRow is a raw scanned work_queue row used to buffer results before secondary lookups.
type wqRow struct {
	artist, title, outdir, filename, outputPathsJSON string
	providerLane, completedAt                        sql.NullString
}

// backfillPlansFromDB enumerates all done work_queue rows in the DB and builds
// a plan for each associated .lrc file that exists on disk.
//
// Two-pass design: the outer rows cursor is closed before any secondary
// lyricsISRCMBID query runs, avoiding a deadlock on the single-connection pool
// (db.Open sets MaxOpenConns(1)).
//
// Returns (plans, skipped, error) where skipped counts .lrc files with no
// on-disk presence and same-stem collisions (MAJOR-2, MAJOR-3).
func backfillPlansFromDB(ctx context.Context, sqlDB *sql.DB) (plans []provenancePlan, skipped int, err error) {
	rows, err := sqlDB.QueryContext(ctx, `
		SELECT id, artist, title, outdir, filename, output_paths, provider_lane, completed_at
		FROM work_queue
		WHERE status = 'done'
		ORDER BY id`)
	if err != nil {
		return nil, 0, fmt.Errorf("query done work_queue rows: %w", err)
	}

	// Pass 1: collect raw rows so we can close the cursor before issuing secondary queries.
	var raw []wqRow
	for rows.Next() {
		var id int64
		var r wqRow
		if err := rows.Scan(&id, &r.artist, &r.title, &r.outdir, &r.filename, &r.outputPathsJSON, &r.providerLane, &r.completedAt); err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("scan work_queue row: %w", err)
		}
		raw = append(raw, r)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close work_queue rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate work_queue rows: %w", err)
	}

	// Pass 2: build plans; secondary DB lookups are safe now that the cursor is closed.
	for _, r := range raw {
		paths := resolveOutputPaths(r.outdir, r.filename, r.outputPathsJSON)
		rec := buildProvenanceRecord(ctx, sqlDB, r.artist, r.title, r.providerLane, r.completedAt)

		for _, p := range paths {
			if strings.ToLower(filepath.Ext(p)) != ".lrc" {
				continue
			}
			if _, serr := os.Stat(p); os.IsNotExist(serr) {
				skipped++ // MAJOR-2: count missing-on-disk files
				continue
			}
			plans = append(plans, makePlan(p, rec))
		}
	}

	// MAJOR-3: detect same-stem collisions (same .lrc path from multiple source tracks).
	// Warn and skip any path that appears more than once to avoid cross-attribution.
	pathCount := make(map[string]int, len(plans))
	for _, p := range plans {
		pathCount[p.Path]++
	}
	warnedPaths := make(map[string]bool)
	var deduped []provenancePlan
	for _, p := range plans {
		if pathCount[p.Path] > 1 {
			if !warnedPaths[p.Path] {
				slog.Warn("backfill: ambiguous .lrc (multiple source tracks); skipping", "path", p.Path)
				warnedPaths[p.Path] = true
				skipped++
			}
			continue
		}
		deduped = append(deduped, p)
	}
	return deduped, skipped, nil
}

// backfillPlansFromPaths walks the given paths/dirs to find .lrc files and
// looks up each in the DB.
//
// Returns (plans, skipped, error) where skipped counts .lrc files with no
// matching DB row (including ambiguous multi-row cases from MAJOR-3).
func backfillPlansFromPaths(ctx context.Context, sqlDB *sql.DB, paths []string) (plans []provenancePlan, skipped int, err error) {
	for _, root := range paths {
		info, err := os.Stat(root)
		if err != nil {
			return nil, 0, fmt.Errorf("stat %s: %w", root, err)
		}
		if !info.IsDir() {
			if strings.ToLower(filepath.Ext(root)) != ".lrc" {
				continue
			}
			if p, ok := lookupPlan(ctx, sqlDB, root); ok {
				plans = append(plans, p)
			} else {
				skipped++ // MAJOR-2: count .lrc files with no usable DB row
			}
			continue
		}
		if werr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || strings.ToLower(filepath.Ext(path)) != ".lrc" {
				return nil
			}
			if p, ok := lookupPlan(ctx, sqlDB, path); ok {
				plans = append(plans, p)
			} else {
				skipped++ // MAJOR-2: count .lrc files with no usable DB row
			}
			return nil
		}); werr != nil {
			return nil, 0, fmt.Errorf("walk %s: %w", root, werr)
		}
	}
	return plans, skipped, nil
}

// canonicalDir returns a canonical form of a directory path for comparison:
// made absolute (a relative path is resolved against the current working
// directory) then Cleaned, which collapses trailing slashes and "." / ".."
// segments. Symlinks are deliberately NOT resolved (the directory may no
// longer exist). On a filepath.Abs error it falls back to filepath.Clean.
func canonicalDir(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs // filepath.Abs already returns a Cleaned path
	}
	return filepath.Clean(p)
}

// lookupPlan looks up a single .lrc file path in the DB and returns its plan.
// MAJOR-3 / NEW-1: ambiguity is measured by DISTINCT work_queue id, not raw
// join-row count. A single track released in multiple audio formats (e.g.
// song.flac + song.mp3) is ONE work_queue row joined to several scan_results
// rows that all point at the same output .lrc; that is unambiguous and tags
// normally. Only genuinely distinct work_queue ids sharing one .lrc path are a
// cross-attribution collision (warn + skip).
func lookupPlan(ctx context.Context, sqlDB *sql.DB, path string) (provenancePlan, bool) {
	base := filepath.Base(path)

	// NEW-2: match the directory by a CANONICAL form rather than an exact SQL
	// string compare. The stored scan_results.outdir may be relative, carry a
	// trailing slash, or otherwise differ in cleanliness from the absolute path
	// the user passes to backfill; an exact `sr.outdir = ?` match would return
	// 0 rows and the file would be silently counted as skipped. We query on the
	// filename alone, then compare canonicalDir(stored outdir) against
	// canonicalDir(query dir) in Go. Symlinks are NOT resolved (EvalSymlinks):
	// the stored dir may no longer exist, so two paths differing only by an
	// unresolved symlink will not match - an accepted residual limitation.
	wantDir := canonicalDir(filepath.Dir(path))

	type wqMatch struct {
		artist, title             string
		providerLane, completedAt sql.NullString
	}

	rows, err := sqlDB.QueryContext(ctx, `
		SELECT wq.id, wq.artist, wq.title, wq.provider_lane, wq.completed_at, sr.outdir
		FROM work_queue wq
		JOIN work_queue_scan_results wqsr ON wqsr.work_queue_id = wq.id
		JOIN scan_results sr ON sr.id = wqsr.scan_result_id
		WHERE sr.filename = ?
		  AND wq.status = 'done'
		ORDER BY wq.id DESC`,
		base,
	)
	if err != nil {
		slog.Warn("backfill: DB lookup failed", "path", path, "error", err)
		return provenancePlan{}, false
	}

	// Collapse to DISTINCT work_queue id: a given wq.id has identical
	// artist/title/provider_lane/completed_at across all its scan_results rows,
	// so keeping the first row seen per id is correct.
	matched := make(map[int64]wqMatch)
	var order []int64 // preserve first-seen order for a stable picked row
	for rows.Next() {
		var id int64
		var m wqMatch
		var storedOutdir string
		if serr := rows.Scan(&id, &m.artist, &m.title, &m.providerLane, &m.completedAt, &storedOutdir); serr != nil {
			_ = rows.Close()
			slog.Warn("backfill: scan row failed", "path", path, "error", serr)
			return provenancePlan{}, false
		}
		// NEW-2: we matched on filename alone; keep only rows whose stored
		// directory canonically matches the directory of the file we are
		// tagging. This rejects same-named files in genuinely different
		// directories while tolerating trailing-slash / relative-vs-absolute /
		// uncleaned differences in the stored outdir.
		if canonicalDir(storedOutdir) != wantDir {
			continue
		}
		if _, seen := matched[id]; !seen {
			matched[id] = m
			order = append(order, id)
		}
	}
	if cerr := rows.Close(); cerr != nil {
		slog.Warn("backfill: close rows failed", "path", path, "error", cerr)
		return provenancePlan{}, false
	}
	if rerr := rows.Err(); rerr != nil {
		slog.Warn("backfill: rows error", "path", path, "error", rerr)
		return provenancePlan{}, false
	}

	switch len(matched) {
	case 0:
		return provenancePlan{}, false
	case 1:
		m := matched[order[0]]
		rec := buildProvenanceRecord(ctx, sqlDB, m.artist, m.title, m.providerLane, m.completedAt)
		return makePlan(path, rec), true
	default:
		// MAJOR-3 / NEW-1: ambiguous - distinct source tracks share this .lrc
		// filename. count reflects distinct tracks, not raw join rows.
		slog.Warn("backfill: ambiguous .lrc (multiple source tracks); skipping", "path", path, "count", len(matched))
		return provenancePlan{}, false
	}
}

// resolveOutputPaths decodes the JSON output_paths blob. Falls back to the
// top-level outdir+filename when the JSON is empty or unparsable.
func resolveOutputPaths(outdir, filename, outputPathsJSON string) []string {
	if strings.TrimSpace(outputPathsJSON) != "" && outputPathsJSON != "[]" && outputPathsJSON != "null" {
		var ops []models.OutputPath
		if err := json.Unmarshal([]byte(outputPathsJSON), &ops); err == nil && len(ops) > 0 {
			paths := make([]string, 0, len(ops))
			for _, op := range ops {
				if op.Outdir != "" && op.Filename != "" {
					paths = append(paths, filepath.Join(op.Outdir, op.Filename))
				}
			}
			if len(paths) > 0 {
				return paths
			}
		}
	}
	if outdir != "" && filename != "" {
		return []string{filepath.Join(outdir, filename)}
	}
	return nil
}

// buildProvenanceRecord assembles the provenance tags from the DB columns plus
// an optional lyrics_cache lookup for ISRC/MBID.
func buildProvenanceRecord(ctx context.Context, sqlDB *sql.DB, artist, title string, providerLane, completedAt sql.NullString) provenanceRecord {
	var rec provenanceRecord
	if providerLane.Valid {
		rec.Source = providerLane.String
	}
	if completedAt.Valid && completedAt.String != "" {
		if t, err := time.Parse(time.RFC3339, completedAt.String); err == nil {
			rec.FetchedAt = t
		}
	}
	// Try to enrich ISRC/MBID from the lyrics_cache JSON blob.
	rec.ISRC, rec.MBID = lyricsISRCMBID(ctx, sqlDB, artist, title)
	return rec
}

// lyricsISRCMBID attempts to read ISRC and RecordingMBID from the lyrics_cache
// JSON blob for the given artist+title. Returns ("", "") when the cache lacks
// the data (partial-coverage case).
func lyricsISRCMBID(ctx context.Context, sqlDB *sql.DB, artist, title string) (isrc, mbid string) {
	var raw string
	err := sqlDB.QueryRowContext(ctx, `
		SELECT lyrics FROM lyrics_cache
		WHERE artist = ? AND title = ?
		ORDER BY updated_at DESC
		LIMIT 1`,
		normalize.NormalizeKey(artist), normalize.NormalizeKey(title),
	).Scan(&raw)
	if err != nil {
		return "", ""
	}
	var song models.Song
	if err := json.Unmarshal([]byte(raw), &song); err != nil {
		return "", ""
	}
	return song.Track.ISRC, song.Track.RecordingMBID
}

// makePlan builds a provenancePlan for one .lrc file from a provenanceRecord.
func makePlan(path string, rec provenanceRecord) provenancePlan {
	var fetched string
	if !rec.FetchedAt.IsZero() {
		fetched = rec.FetchedAt.Format(time.RFC3339)
	}
	tags := lyrics.ProvenanceTags{
		Source:  rec.Source,
		Fetched: fetched,
		ISRC:    rec.ISRC,
		MBID:    rec.MBID,
	}
	partial := rec.Source == "" || rec.FetchedAt.IsZero()
	return provenancePlan{Path: path, Tags: tags, Partial: partial}
}
