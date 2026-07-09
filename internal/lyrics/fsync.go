package lyrics

import (
	"log/slog"
	"os"
)

// fsyncDir flushes the directory entry for dir to stable storage so that a
// rename into it (the final step of our atomic write/rewrite) survives a hard
// crash. The temp file's data is already Sync'd before the rename; this fsyncs
// the parent so the rename itself is durable.
//
// NEW-3: a failure here is non-fatal and durability-only - the rename has
// already succeeded, so the new content is visible; only crash-durability is at
// risk. On platforms where opening a directory for sync is unsupported (e.g.
// Windows), the open or Sync call fails and we warn + continue rather than
// failing the whole operation.
func fsyncDir(dir string) { FsyncDir(dir) }

// FsyncDir is the exported form of fsyncDir, so callers outside this package
// (e.g. the realign command's clobber-safe rename) get the same crash-durable
// parent-directory flush after an os.Rename. Semantics are identical: failures
// are non-fatal and durability-only.
func FsyncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // dir is the parent of a caller-controlled output path already validated upstream
	if err != nil {
		slog.Warn("fsync parent dir: open failed (durability-only, continuing)", "dir", dir, "error", err)
		return
	}
	if serr := d.Sync(); serr != nil {
		slog.Warn("fsync parent dir: sync failed (durability-only, continuing)", "dir", dir, "error", serr)
	}
	if cerr := d.Close(); cerr != nil {
		slog.Warn("fsync parent dir: close failed", "dir", dir, "error", cerr)
	}
}
