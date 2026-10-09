package reports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
// configured library roots (see LibraryRoots) before reading a byte, by
// opening it through an os.Root on the matching root (as the web layer's
// openPreviewAudio does), so no component can resolve outside the root AT
// the open. Confinement is deliberately not done here, and a lexical check
// (pathutil.WithinRoot) is never enough: stored roots are neither cleaned nor
// symlink-resolved, and a check-then-open races a swapped directory.
type PreviewTarget struct {
	ID     int64
	Artist string
	Title  string
	Album  string
	Status string
	// ArtistKey and TitleKey are the row's own work_queue identity keys, the
	// identity lyric blocks are recorded under (#1399).
	ArtistKey string
	TitleKey  string
	SyncTier  string // "word", "line", "unsynced" or "" when not yet classified
	// LineEditable reports a settled synced row at the current line-synced
	// rung (lineTierPredicate), the only rows the offset editor may rewrite
	// (#481 Stage 2). Decided in SQL from the row's recorded state.
	LineEditable bool
	// ManualInstrumental, Blocked and HasLyric are the mark-action state the
	// player shows (#1250), read from the row's recorded state exactly as the
	// list screens do (manual_instrumental_at, a lyric_blocks row for the
	// identity keys, and an outcome that says a lyric file was written).
	ManualInstrumental, Blocked, HasLyric bool

	// AudioPath is work_queue.source_path, whitespace-trimmed ("" when the row
	// has none). Sidecars are probed only when it is absolute; otherwise both
	// sidecar paths are empty, since a relative path would resolve against the
	// server's working directory.
	AudioPath string
	// LRCPath is the line-synced sidecar that exists beside the audio: the
	// exact stem+".lrc" name, else the first extension-case variant on disk
	// (sidecar.ResolveCaseVariant, the same resolver revalidate uses). Empty
	// when no regular .lrc exists. A symlink is never reported.
	LRCPath string
	// ELRCCandidate is the word-synced companion NAME beside LRCPath that the
	// writer's exact-name rule would pair (lyrics.OwnedCompanionOf), found with
	// Lstat only. It is a CANDIDATE, not a claim: PreviewSource never opens it,
	// so it makes NO ownership statement. If stem+".elrc" exists in any form
	// (Lstat reports anything but not-exist), that exact name alone decides: it
	// is the candidate when it is a regular file, and otherwise there is none
	// and no extension-case variant is consulted, exactly as the writer would
	// never pair one. Only when the exact name is absent is the first regular
	// extension-case variant (sidecar.ResolveCaseVariant, no directory read)
	// reported. Empty when there is no LRCPath or no such regular file.
	//
	// A consumer MUST, before using it: open it through an os.Root on the
	// matching LibraryRoots entry (confinement at the open, so a symlink
	// swapped in after the Lstat cannot escape), and confirm
	// canticle owns it (lyrics.IsOwnedCompanion semantics) with a BOUNDED
	// header read, treating a foreign or unreadable file as no companion. One
	// known divergence the consumer inherits: when several variants exist and
	// the first in name order is foreign while a later one is owned, the writer
	// would pair the later one; the preview reports only the first and so shows
	// none. That shape needs two case-variant companions side by side and is
	// left to the consumer's ownership check to fail closed on.
	//
	// An I/O error (EACCES, EIO, an unavailable mount) reads as ABSENT for both
	// paths (an unstatable exact .elrc still blocks the variants): this is a read-only preview and does not distinguish "no
	// lyrics" from "could not tell".
	ELRCCandidate string
}

// PreviewSource loads the preview description of work_queue row id. It reads
// one row, Lstats the .lrc name and, when that misses, up to 7 extension-case
// variants; with a .lrc found it Lstats the exact .elrc name and, only when
// that name is absent, up to 15 variants. It is Lstat-ONLY: it never opens,
// reads, or lists anything on disk, so a DB-derived path cannot make it read
// an out-of-root or arbitrarily large file (confinement and ownership are the
// consumer's job, see PreviewTarget.ELRCCandidate). A missing row yields
// ErrPreviewNotFound.
//
// This is a single-row, user-triggered call (one click, with the audio about
// to stream from the same disk). It must NEVER be called per row of a list
// view: the stats wake disks and cost O(rows), the exact
// shape #684 removed from the scan path.
//
// No production caller yet: its consumer is slice 481-5 (the player page, #481).
func (r *Repo) PreviewSource(ctx context.Context, id int64) (PreviewTarget, error) {
	t := PreviewTarget{ID: id}
	err := r.db.QueryRowContext(ctx,
		`SELECT artist, title, album, status, COALESCE(sync_tier, ''), source_path, artist_key, title_key,
		        COALESCE(`+lineEditableSQL+`, 0), `+manualMarkPredicate+`, `+blockedExistsSQL+`, `+hasLyricSQL+`
		   FROM work_queue WHERE id = ?`, id,
	).Scan(&t.Artist, &t.Title, &t.Album, &t.Status, &t.SyncTier, &t.AudioPath, &t.ArtistKey, &t.TitleKey, &t.LineEditable,
		&t.ManualInstrumental, &t.Blocked, &t.HasLyric)
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
		t.ELRCCandidate = elrcCandidate(sidecar.StemOf(t.LRCPath) + sidecar.ExtWordSynced)
	}
	return t, nil
}

// PreviewAudioPath returns only the trimmed source_path of work_queue row id,
// for the audio route, which a browser hits once per Range request: it touches
// no file, so seeking does not repeat PreviewSource's sidecar Lstats. A missing
// row yields ErrPreviewNotFound; a blank path yields "".
func (r *Repo) PreviewAudioPath(ctx context.Context, id int64) (string, error) {
	var p string
	err := r.db.QueryRowContext(ctx, `SELECT source_path FROM work_queue WHERE id = ?`, id).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPreviewNotFound
	}
	if err != nil {
		return "", fmt.Errorf("preview audio path %d: %w", id, err)
	}
	return strings.TrimSpace(p), nil
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

// elrcCandidate applies the writer's exact-name rule (lyrics.ownedCompanionOfErr)
// with Lstat only: an exact name that exists in ANY form (regular, symlink,
// directory, or unstatable) alone decides and is reported only when regular;
// variants are probed only when the exact name does not exist. Opens nothing.
func elrcCandidate(exact string) string {
	fi, err := os.Lstat(exact)
	if !os.IsNotExist(err) {
		if err == nil && fi.Mode().IsRegular() {
			return exact
		}
		return ""
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
// (not cleaned, symlinks unresolved), so a consumer must never trust a lexical
// match alone: open through an os.Root on the root so no component can escape.
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
