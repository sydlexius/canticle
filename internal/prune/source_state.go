package prune

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// SourceState is what SourceStateOf could prove about one source file.
type SourceState int

const (
	// SourceUnproven means no proof either way. A stat error other than not-exist, an
	// offline or unconfigured root, a root symlink that did not resolve, or a
	// subtree that shows a sub-mount went away all land here.
	SourceUnproven SourceState = iota
	// SourcePresent means the file exists.
	SourcePresent
	// SourceGone means the file does not exist and the evidence that the library is
	// merely unavailable is absent (see SourceStateOf).
	SourceGone
)

// SourceStateOf is the one definition of "this source file is provably gone"
// the unattended worker uses (#1597), built from the same guards as
// provablyStale so the two cannot disagree. Downtime is not absence: a file is
// SourceGone only when its lstat is not-exist AND
//
//   - a configured library root contains it, that root's symlink resolved on THIS
//     call, and the root is online by a FRESH probe (never a cached answer, which
//     may predate the mount dropping); and
//   - subtreeIntact holds for the file's directory: no symlink or non-directory
//     on the way down, and the nearest existing ancestor is populated, so an
//     empty sub-mountpoint inside a live root is not mistaken for a removal.
//
// Every doubt answers SourceUnproven. It cannot see a union mount that lost one
// branch while the root stays populated, so the caller must confirm over time
// rather than act on one sighting.
func (p *Pruner) SourceStateOf(ctx context.Context, sourcePath string) (SourceState, error) {
	if sourcePath == "" {
		return SourceUnproven, nil
	}
	_, err := os.Lstat(sourcePath)
	switch {
	case err == nil:
		return SourcePresent, nil
	case !errors.Is(err, fs.ErrNotExist):
		return SourceUnproven, nil
	}
	roots, err := p.LibraryRoots(ctx)
	if err != nil {
		return SourceUnproven, err
	}
	rs := newRootSet(roots)
	root, ok := rs.rootOf(sourcePath)
	if !ok || rs.unresolved[root] || !p.RootOnline(root) {
		return SourceUnproven, nil
	}
	spelling, ok := rs.match(sourcePath)
	if !ok || !subtreeIntact(spelling, filepath.Dir(sourcePath)) {
		return SourceUnproven, nil
	}
	return SourceGone, nil
}
