// Package upstreamrepair holds the decision logic of `scan reconcile-upstream`
// (#1298): which upstream a settled row gets from its sidecar's tags, and the
// guarded, write-ahead-backed apply loop. The package takes a queue (an
// interface) and a backup Sink instead of a config path, never opens a
// database and never prints; the command keeps config loading, the database
// open and the aggregate stdout.
//
// Library paths, artists, titles and lyric lines never reach a count, a log
// field, a backup record or a log line. The one path an error may name is the
// operator's own backup file, and it is never logged here.
package upstreamrepair

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/canticle/internal/innertube"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/revalidate"
	"github.com/sydlexius/canticle/internal/tierrepair"
)

// BackupRecord is one JSONL line per filled row: the id, lane and value
// written. No path, artist or title. The prior value is always NULL, so
// restoring is "set upstream back to NULL".
type BackupRecord struct {
	ID       int64  `json:"id"`
	Lane     string `json:"lane"`
	Upstream string `json:"upstream"`
}

// Outcomes of Plan other than a fill, plus the one the apply loop adds.
const (
	OutcomeFill           = "fill"
	OutcomeProcessing     = "processing"
	OutcomeNoSidecar      = "no_sidecar"
	OutcomeUnreadable     = "unreadable"
	OutcomeNoTags         = "no_tags"
	OutcomeNoUpstream     = "no_upstream"
	OutcomeUnknownUp      = "unknown_upstream"
	OutcomeSourceMismatch = "source_mismatch"
	OutcomeRaced          = "raced"
)

var (
	// ErrNoBackup reports an applying run given no backup Sink. A run that
	// changes rows without a backup is refused, never silently unbacked.
	ErrNoBackup = tierrepair.ErrNoBackup
	// ErrBackupWrite marks a failed backup record write, so a caller can log a
	// fixed reason instead of an error whose text names the backup file.
	ErrBackupWrite = errors.New("upstreamrepair: backup record write failed")
)

// Sink receives the write-ahead backup records; *os.File satisfies it.
type Sink = tierrepair.Sink

// Queue is the queue surface the backfill needs. *queue.DBQueue satisfies it;
// a test may fake the write.
type Queue interface {
	ListUpstreamCandidates(ctx context.Context, lanes []string) ([]queue.UpstreamCandidate, error)
	SetUpstreamIfPending(ctx context.Context, c queue.UpstreamCandidate, upstream string, backup func() error) (bool, error)
}

// List returns the done or processing rows on an upstream-reporting lane with
// no upstream yet. Its length is the pending count; it opens no library file.
func List(ctx context.Context, q Queue) ([]queue.UpstreamCandidate, error) {
	return q.ListUpstreamCandidates(ctx, providers.UpstreamLanes()) //nolint:wrapcheck // reason: the caller logs the list failure with its own context
}

// Progress receives done rows of total; it must not block.
type Progress = tierrepair.Progress

// Options control one run. A dry run (Apply false) only counts and ignores
// Backup.
type Options = tierrepair.Options

// Result is a run's counts. Counts is keyed by Plan's outcomes plus "raced";
// WriteFailed counts guarded writes that errored; Applied counts rows filled.
// CancelFailed is the subset of WriteFailed whose write errored while ctx was
// already canceled: the cancel, not the row, failed it. The command ignores it
// (its stdout keeps counting them in WriteFailed).
type Result struct {
	Scanned      int
	Counts       map[string]int
	WriteFailed  int
	CancelFailed int
	Applied      int
}

// Run plans each candidate and, when o.Apply, fills the upstream through q.
// Backup-first: each record is written inside SetUpstreamIfPending's own
// transaction before the fill commits, so a raced row never gets a backup
// call. A write failure counts separately and does not stop the remaining
// rows. When ctx is canceled it stops at the next row boundary and returns the
// partial result with an error wrapping ctx.Err(); no row is half-applied.
func Run(ctx context.Context, q Queue, candidates []queue.UpstreamCandidate, o Options) (Result, error) {
	res := Result{Scanned: len(candidates), Counts: map[string]int{}}
	if o.Apply && o.Backup == nil {
		return res, ErrNoBackup
	}
	for i, c := range candidates {
		if cerr := ctx.Err(); cerr != nil {
			return res, fmt.Errorf("upstream repair stopped: %w", cerr)
		}
		upstream, outcome := Plan(c)
		res.Counts[outcome]++
		if outcome == OutcomeFill && o.Apply {
			ok, err := q.SetUpstreamIfPending(ctx, c, upstream, func() error {
				return appendBackup(o.Backup, c, upstream)
			})
			switch {
			case err != nil:
				logWriteFailure(c.ID, err)
				res.Counts[outcome]--
				res.WriteFailed++
				if ctx.Err() != nil {
					res.CancelFailed++
				}
			case !ok:
				res.Counts[outcome]--
				res.Counts[OutcomeRaced]++
			default:
				res.Applied++
			}
		}
		if o.Progress != nil {
			o.Progress(int64(i+1), int64(len(candidates)))
		}
	}
	return res, nil
}

// logWriteFailure logs a fixed reason for a backup failure (its error text
// names the operator's backup file) and the error for a queue failure.
func logWriteFailure(id int64, err error) {
	if errors.Is(err, ErrBackupWrite) {
		slog.Warn("reconcile-upstream: write failed; leaving row unchanged", "id", id, "reason", "backup write failed")
		return
	}
	slog.Warn("reconcile-upstream: write failed; leaving row unchanged", "id", id, "error", err)
}

// Plan decides one candidate, shared by the dry run and the apply. On a
// quiescent database their counts are equal: raced and write_failed can only be
// non-zero on apply, and "would fill" includes rows that may still race. The
// sidecar is DERIVED from source_path (stem + .lrc, else the owned .elrc, else
// .txt; a foreign .elrc is skipped, by lyrics.IsOwnedCompanion), each with the
// extension-case resolver as its miss fallback; the first regular file found is
// the one read, and a tagless one does not fall through to the next. It is read
// through lyrics.ReadProvenanceHeader (no-follow, regular-file-only, bounded),
// so a symlink or FIFO is never read. Returns (upstream, OutcomeFill) only when
// the sidecar's [source:] equals the row's lane and it carries a KNOWN
// [upstream:] token; anything else is counted and left NULL, never guessed.
func Plan(c queue.UpstreamCandidate) (upstream, outcome string) {
	if c.Status == "processing" {
		return "", OutcomeProcessing
	}
	audio := strings.TrimSpace(c.AudioPath)
	if audio == "" {
		return "", OutcomeNoSidecar
	}
	stem := strings.TrimSuffix(audio, filepath.Ext(audio))
	for _, ext := range []string{".lrc", ".elrc", ".txt"} {
		path := stem + ext
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			var variant string
			var vfi os.FileInfo
			var ok bool
			if variant, vfi, ok = revalidate.ResolveSidecarCaseVariant(path); !ok {
				continue
			}
			path, fi, err = variant, vfi, nil
		}
		switch {
		case err != nil:
			return "", OutcomeUnreadable
		case !fi.Mode().IsRegular():
			return "", OutcomeUnreadable
		}
		if ext == ".elrc" && !lyrics.IsOwnedCompanion(path) {
			continue
		}
		pt, rerr := lyrics.ReadProvenanceHeader(path)
		switch {
		case rerr != nil:
			return "", OutcomeUnreadable
		case pt.Source == "":
			return "", OutcomeNoTags
		case pt.Source != c.Lane:
			return "", OutcomeSourceMismatch
		case pt.Upstream == "":
			return "", OutcomeNoUpstream
		case !innertube.KnownUpstream(pt.Upstream):
			return "", OutcomeUnknownUp
		}
		return pt.Upstream, OutcomeFill
	}
	return "", OutcomeNoSidecar
}

// appendBackup writes and fsyncs one record, write-ahead from inside the row's
// transaction. A write or sync failure wraps ErrBackupWrite.
func appendBackup(s Sink, c queue.UpstreamCandidate, upstream string) error {
	b, err := json.Marshal(BackupRecord{ID: c.ID, Lane: c.Lane, Upstream: upstream})
	if err != nil {
		return fmt.Errorf("marshal reconcile-upstream backup record: %w", err)
	}
	if _, err := s.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write reconcile-upstream backup record: %w: %w", ErrBackupWrite, err)
	}
	if err := s.Sync(); err != nil {
		return fmt.Errorf("sync reconcile-upstream backup record: %w: %w", ErrBackupWrite, err)
	}
	return nil
}
