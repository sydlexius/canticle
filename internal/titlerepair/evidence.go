package titlerepair

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// The cull of one candidate. lyricblock.Service.Mark does all of it, in its own
// crash-safe order: the backup record, the block, the cache invalidation, the
// unlink and the reopen. This file adds the evidence a file must show before
// Mark may touch it, and maps Mark's answer to a counter. It writes nothing.

// opName labels the backup records of this repair.
const opName = "reconcile-instrumental-titles"

// mtimeSlack is how far either side of the row's completed_at a file's mtime may
// sit and still be the file that settle wrote. Two-sided on purpose: a file
// copied in with an old timestamp is no more the row's than one written later.
const mtimeSlack = 2 * time.Minute

// The vet's refusals. Mark wraps them as they are, so they name no file.
var (
	errSourceDisagrees = errors.New("titlerepair: a lyric file's source tag names another lane")
	errNotProvable     = errors.New("titlerepair: a lyric file is not provably the row's own")
)

// cull hands one candidate to Mark (a dry run unless o.Apply), counts the
// outcome and returns the row's disposition and the files Mark read for it.
// noBlock culls a row Strong by album with no block (MarkRequest.NoBlock).
func (r *Repairer) cull(ctx context.Context, c candidate, o Options, res *Result, noBlock bool) (string, []string) {
	var files []string
	mr, err := r.svc.Mark(ctx, lyricblock.MarkRequest{
		WorkItemID: c.id, Roots: o.Roots, DryRun: !o.Apply, Report: o.Report, Op: opName,
		RefuseHandEdited: true, RequireEmptyStem: true, NoBlock: noBlock,
		Vet: func(recs []lyricblock.Backup) error {
			for _, b := range recs {
				if !slices.Contains(files, b.Path) {
					files = append(files, b.Path)
				}
			}
			return vet(c, recs)
		},
	})
	// A refusal changes nothing and returns a zero result. A non-zero result
	// with an error is a mark that stopped part-way, whatever the error wraps.
	changed := mr != (lyricblock.MarkResult{})
	switch {
	case err == nil && !o.Apply:
		res.WouldRepair++
		res.BackedUp += len(files)
		return DispositionWouldRepair, files
	case err == nil:
		res.Repaired++
		res.BackedUp += len(files) // every file vetted, one that appeared mid-mark too
		res.Removed += mr.Removed
		res.NewBlocks += mr.NewBlocks
		res.CacheInvalidated += mr.CacheInvalidated
		if mr.Reopened {
			res.Reopened++
		}
		return DispositionRepaired, files
	case changed && (errors.Is(err, errSourceDisagrees) || errors.Is(err, errNotProvable)),
		mr.Removed == 0 && errors.Is(err, lyricblock.ErrChangedBeforeRemoval):
		// Mark's late vet (see MarkRequest.Vet): blocked, nothing removed, and a
		// rerun refuses the row at the first vet. Not retryable.
		// A file replaced after its backup stops the same way; a rerun vets it.
		res.NeedsManualMark++
		r.log.Warn("titlerepair: a file appeared or changed mid-repair and was not vetted; mark the row wrong by hand", "id", c.id)
		return DispositionNeedsManualMark, files
	case changed: // an error below, whatever it wraps: the row is part-way, not skipped
	case errors.Is(err, lyricblock.ErrBusy):
		res.Candidates--
		res.InFlight++
		return DispositionInFlight, files
	case errors.Is(err, lyricblock.ErrHandEdited), errors.Is(err, lyricblock.ErrManualInstrumental):
		res.Candidates--
		res.HandMarked++
		return DispositionHandMarked, files
	default:
		for _, skip := range []struct {
			err         error
			count       *int
			disposition string
		}{
			{lyricblock.ErrSymlinkedSidecar, &res.Symlink, DispositionSymlink},
			{lyricblock.ErrOutsideRoots, &res.OutsideRoots, DispositionOutsideRoots},
			{lyricblock.ErrStemOccupied, &res.StemOccupied, DispositionStemOccupied},
			{lyricblock.ErrNoSidecar, &res.NoFile, DispositionNoFile},
			{lyricblock.ErrNoFingerprint, &res.NoFile, DispositionNoFile},
			{errSourceDisagrees, &res.SourceDisagrees, DispositionSourceDisagrees},
			{errNotProvable, &res.NotProvable, DispositionNotProvable},
		} {
			if errors.Is(err, skip.err) {
				*skip.count++
				return skip.disposition, files
			}
		}
	}
	// The cause is not logged: a Report error is the caller's and may name a path.
	res.Errors++
	r.log.Error("titlerepair: repair of a row failed; a rerun reports where it stands", "id", c.id)
	return DispositionError, files
}

// vet is Mark's Vet hook for row c: every file must be the one the row's settle
// wrote. A .lrc whose [source:] tag names another lane is refused (an empty tag
// asserts nothing); the tag is parsed from the record's bytes, the ones its time
// dates, never from a second read of the path. Then one rule, with no way round
// it: the file's modification time is within mtimeSlack of the row's
// completed_at, before or after. A row with no readable completed_at, or a
// record with no time, cannot show that and is refused. Words prove nothing: a
// fingerprint is letters and digits only, so a file edited outside canticle
// still matches the cached lyric. The cost: a row re-settled without a write
// (the worker restamps completed_at when it keeps the file on disk), or a file
// rewritten in place later, is skipped as not provable. A repair that crashed
// after Mark's first transaction left both dates alone, so a rerun passes. It
// never changes the records: the backup holds them.
func vet(c candidate, recs []lyricblock.Backup) error {
	// An unparsable completed_at refuses every file. The layout is the queue's.
	settled, settledErr := time.Parse(time.RFC3339, c.completedAt)
	for _, b := range recs {
		if sidecar.KindOf(b.Path) == sidecar.KindLineSynced {
			tags, terr := lyrics.ProvenanceFromBytes(b.Content)
			if terr != nil {
				return errNotProvable
			}
			if tags.Source != "" && !strings.EqualFold(tags.Source, strings.TrimSpace(c.lane)) {
				return errSourceDisagrees
			}
		}
		// The time of the handle the bytes came from; a record without one refuses.
		if settledErr != nil || b.ModTime.IsZero() {
			return errNotProvable
		}
		if off := b.ModTime.Sub(settled); off > mtimeSlack || off < -mtimeSlack {
			return errNotProvable
		}
	}
	return nil
}
