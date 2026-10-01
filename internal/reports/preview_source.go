package reports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// ErrPreviewNotFound is returned by PreviewSource when no work_queue row has
// the requested id.
var ErrPreviewNotFound = errors.New("reports: work_queue row not found")

// PreviewTarget describes one work_queue row for the web preview (#481): its
// display fields plus the on-disk paths of its audio file and lyric sidecars.
//
// This is PURE DATA. Nothing here opens, serves, or confines a path: every
// path comes straight from a database column (source_path) or is derived from
// it, so a caller that serves file content MUST confine each path to the
// configured library roots (see LibraryRoots) and open it without following
// symlinks before reading a byte (O_NOFOLLOW). Confinement is deliberately not
// done here, and a consumer must use pathutil.ResolveWithinRoot, which cleans
// and resolves both sides, never the lexical pathutil.WithinRoot: stored roots
// are neither cleaned nor symlink-resolved.
type PreviewTarget struct {
	ID       int64
	Artist   string
	Title    string
	Album    string
	Status   string
	SyncTier string // "word", "line", "unsynced" or "" when not yet classified

	// AudioPath is work_queue.source_path, whitespace-trimmed ("" when the row
	// has none). Sidecars are probed only when it is absolute; otherwise both
	// sidecar paths are empty, since a relative path would resolve against the
	// server's working directory.
	AudioPath string
	// LRCPath is the line-synced sidecar that exists beside the audio: the
	// exact stem+".lrc" name, else the first extension-case variant on disk
	// (sidecar.ResolveCaseVariant, the same resolver revalidate uses). Empty when no regular .lrc exists. A symlink is never reported.
	LRCPath string
	// ELRCPath is the word-synced companion canticle OWNS beside LRCPath: the
	// exact stem+".elrc" or an extension-case variant, kept only when
	// lyrics.IsOwnedCompanion (the writer's own ownership predicate) agrees.
	// Empty when there is no LRCPath, no companion, or the companion is foreign.
	//
	// An I/O error (EACCES, EIO, an unavailable mount) reads as ABSENT for both
	// paths: this is a read-only preview and does not distinguish "no lyrics"
	// from "could not tell".
	ELRCPath string
}

// PreviewSource loads the preview description of work_queue row id. It reads
// one row, Lstats the .lrc name and, when that misses, up to 7 extension-case
// variants; with a .lrc found it does the same for the .elrc (up to 15
// variants) and, if one exists, OPENS it to read its header for the ownership
// check (lyrics.IsOwnedCompanion). It never lists a directory. A missing row
// yields ErrPreviewNotFound.
//
// This is a single-row, user-triggered call (one click, with the audio about
// to stream from the same disk). It must NEVER be called per row of a list
// view: the stats and the header read wake disks and cost O(rows), the exact
// shape #684 removed from the scan path.
func (r *Repo) PreviewSource(ctx context.Context, id int64) (PreviewTarget, error) {
	t := PreviewTarget{ID: id}
	err := r.db.QueryRowContext(ctx,
		`SELECT artist, title, album, status, COALESCE(sync_tier, ''), source_path
		   FROM work_queue WHERE id = ?`, id,
	).Scan(&t.Artist, &t.Title, &t.Album, &t.Status, &t.SyncTier, &t.AudioPath)
	if errors.Is(err, sql.ErrNoRows) {
		return PreviewTarget{}, ErrPreviewNotFound
	}
	if err != nil {
		return PreviewTarget{}, fmt.Errorf("preview source %d: %w", id, err)
	}
	t.AudioPath = strings.TrimSpace(t.AudioPath)
	if !filepath.IsAbs(t.AudioPath) {
		return t, nil
	}
	t.LRCPath = resolveRegular(sidecar.StemOf(t.AudioPath) + sidecar.ExtLineSynced)
	if t.LRCPath != "" && sidecar.Active(sidecar.KindWordSynced) {
		if c := resolveRegular(sidecar.StemOf(t.LRCPath) + sidecar.ExtWordSynced); c != "" && lyrics.IsOwnedCompanion(c) {
			t.ELRCPath = c
		}
	}
	return t, nil
}

// resolveRegular returns exact when it is a regular file (Lstat, so a symlink
// is not one), else the extension-case variant sidecar.ResolveCaseVariant
// finds (bounded Lstats, no directory read), else "".
func resolveRegular(exact string) string {
	if fi, err := os.Lstat(exact); err == nil && fi.Mode().IsRegular() {
		return exact
	}
	if variant, _, ok := sidecar.ResolveCaseVariant(exact); ok {
		return variant
	}
	return ""
}

// LibraryRoots returns every configured library root path, read live from the
// libraries table on each call so a root added or removed while serving takes
// effect without a restart. Ordered by id for determinism. It is the input a
// caller's path confinement checks against, but the paths are stored as given
// (not cleaned, symlinks unresolved), so a consumer must confine with
// pathutil.ResolveWithinRoot (never the lexical WithinRoot) and open with
// O_NOFOLLOW.
func (r *Repo) LibraryRoots(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT path FROM libraries ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("library roots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var roots []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("library roots: scan: %w", err)
		}
		roots = append(roots, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("library roots: %w", err)
	}
	return roots, nil
}
