package prune

import (
	"context"

	"github.com/sydlexius/canticle/internal/models"
)

// multiPlan is planMultiRepair's verdict for one multi-entry output_paths.
type multiPlan struct {
	kept     []models.OutputPath // the list after the drops; nil when nothing drops
	dropped  int
	retained int  // missing entries deliberately left in place
	statErr  bool // an entry's directory could not be statted: nothing proven
}

// planMultiRepair decides which MISSING-directory entries of a multi-entry row
// may be dropped (#1430). Only the attended, backup-first reconcile calls it;
// the unattended worker never removes an entry.
//
// A row's output_paths holds one entry per file that collapsed into it
// (identityrepair's union, and the work_queue dedupe on artist/title), so an
// entry with a missing directory is either a stale pre-relink copy or a REAL
// second copy of the song whose folder is momentarily gone (an unmounted
// library, an album folder mid-rename). A missing entry is dropped only when
// provablyStale holds (see EntryProvablyStale for exactly what that proves and
// does not); every other missing entry is retained and counted. The list is
// never emptied: with no entry whose directory exists (a non-directory does
// not count as a destination), nothing is dropped.
func (p *Pruner) planMultiRepair(ctx context.Context, id int64, sourcePath string, paths []models.OutputPath) (multiPlan, error) {
	var missing []int
	present := 0
	for i, e := range paths {
		switch statDir(e.Outdir) {
		case dirMissing:
			missing = append(missing, i)
		case dirStatError:
			return multiPlan{statErr: true}, nil
		case dirPresent:
			present++
		}
	}
	if len(missing) == 0 {
		return multiPlan{}, nil
	}
	if present == 0 {
		return multiPlan{retained: len(missing)}, nil
	}
	libRoots, err := p.LibraryRoots(ctx)
	if err != nil {
		return multiPlan{}, err
	}
	roots := newRootSet(libRoots)
	linked, links, err := p.linkedFiles(ctx, id, sourcePath)
	if err != nil {
		return multiPlan{}, err
	}
	drop := map[int]bool{}
	var pl multiPlan
	for _, i := range missing {
		if !provablyStale(roots, p.RootOnline, linked, links, paths[i].Outdir) {
			pl.retained++
			continue
		}
		drop[i] = true
		pl.dropped++
	}
	if pl.dropped == 0 {
		return pl, nil
	}
	for i, e := range paths {
		if !drop[i] {
			pl.kept = append(pl.kept, e)
		}
	}
	return pl, nil
}
