package lyricblock

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/selfwrite"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// The per-track backup-first cull: inventory a track's lyric files, capture
// their bytes through a no-follow handle, hand each to a durable Report, then
// unlink. Nothing here is block-specific. Errors never carry a path (private
// library metadata).

// MaxBackupBytes caps the size of a lyric file the backup will capture. A larger
// file fails the backup, which (backup-first) leaves everything untouched.
const MaxBackupBytes = 4 << 20

// ErrSymlinkedSidecar is returned when a lyric file is a symlink: it can be
// neither followed nor backed up, so the cull refuses before changing anything.
var ErrSymlinkedSidecar = errors.New("lyricblock: a lyric file is a symlink")

// ErrOutsideRoots is returned when an output directory is not inside any of the
// supplied library roots. Nothing is touched.
var ErrOutsideRoots = errors.New("lyricblock: output is outside the library roots")

// Output is one resolved output location of a track: a directory (already
// confined to a library root) and the audio filename its sidecars are named for.
type Output struct{ Dir, Filename string }

// Backup is one restorable JSONL line: the bytes of a lyric file about to be
// removed. Restoring is writing Content back to Path. Meta is free-form state
// the caller wants a restore to have (omitted when empty).
type Backup struct {
	Op         string            `json:"op"`
	WorkItemID int64             `json:"work_item_id"`
	Path       string            `json:"path"`
	Content    []byte            `json:"content"`
	Meta       map[string]string `json:"meta,omitempty"`
}

// AppendBackup writes b to f as one JSON line and fsyncs it, so the record is
// durable before the removal it protects. It is the body of a caller's Report.
func AppendBackup(f *os.File, b Backup) error {
	line, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("lyricblock: marshal backup record: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("lyricblock: write backup record: %w", stripPath(err))
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("lyricblock: sync backup record: %w", stripPath(err))
	}
	return nil
}

// Inventory lists the existing lyric files for a track's outputs: every case
// variant of the .lrc and .txt, plus every OWNED .elrc companion whether or not
// a .lrc sits beside it (the writer's own ownership rule decides; a companion
// canticle does not own is never listed). A symlink is ErrSymlinkedSidecar.
func Inventory(artist, title string, outs []Output) ([]string, error) {
	var files []string
	seen := make(map[string]bool)
	add := func(p string) error {
		fi, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("lyricblock: stat lyric file: %w", stripPath(err))
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return ErrSymlinkedSidecar
		}
		// A hand-placed instrumental marker is not a lyric.
		if lyrics.ManualMarkerOnDisk(p) {
			return nil
		}
		if !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
		return nil
	}
	for _, o := range outs {
		l := sidecar.List(o.Dir)
		var lrcFp string
		for _, synced := range []bool{true, false} {
			name, err := lyrics.SidecarName(artist, title, o.Filename, synced)
			if err != nil {
				return nil, fmt.Errorf("lyricblock: sidecar name: %w", err)
			}
			fp := filepath.Join(o.Dir, name)
			if synced {
				lrcFp = fp
			}
			for _, v := range l.Variants(fp) {
				if err := add(v); err != nil {
					return nil, err
				}
			}
		}
		for _, c := range lyrics.OwnedCompanions(lrcFp, l) {
			if err := add(c); err != nil {
				return nil, err
			}
		}
	}
	return files, nil
}

// ReadBackup captures path's bytes through one no-follow handle that must fstat
// regular, so a file swapped for a symlink after the inventory is refused.
func ReadBackup(op string, workItemID int64, path string) (Backup, error) {
	b, err := lyrics.ReadRegularNoFollow(path, MaxBackupBytes)
	if err != nil {
		return Backup{}, fmt.Errorf("read lyric file (not regular, over the %d-byte backup limit, or unreadable): %w", MaxBackupBytes, stripPath(err))
	}
	return Backup{Op: op, WorkItemID: workItemID, Path: path, Content: b}, nil
}

// RemoveFiles unlinks paths, owned word-synced companions first (the writer's
// rule: a failed companion removal leaves its .lrc too, so a pair is never
// split). Every path is recorded with sw first (nil is a no-op) so the watcher
// ignores the deletions. It stops at the first failure, reporting how many were
// removed; a file already gone counts as removed.
func RemoveFiles(paths []string, sw *selfwrite.Registry) (int, error) {
	ordered := append([]string(nil), paths...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return sidecar.KindOf(ordered[i]) == sidecar.KindWordSynced && sidecar.KindOf(ordered[j]) != sidecar.KindWordSynced
	})
	sw.Record(ordered...)
	for i, p := range ordered {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return i, stripPath(err)
		}
	}
	return len(ordered), nil
}

// stripPath drops the path an os error carries, keeping only its cause.
func stripPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

// resolveOutputs returns each output's symlink-resolved directory, confined to
// one of roots.
func resolveOutputs(roots []string, outs []models.OutputPath) ([]Output, error) {
	res := make([]Output, len(outs))
	for i, o := range outs {
		for _, r := range roots {
			if d, ok := pathutil.ResolveWithinRoot(r, o.Outdir); ok {
				res[i] = Output{Dir: d, Filename: o.Filename}
				break
			}
		}
		if res[i].Dir == "" {
			return nil, ErrOutsideRoots
		}
	}
	return res, nil
}

// reportedSet remembers the content (sha256) last reported for each path.
type reportedSet map[string][sha256.Size]byte

// report sends rec to fn unless this exact path and content was already sent.
func report(fn func(Backup) error, rec Backup, reported reportedSet) error {
	sum := sha256.Sum256(rec.Content)
	if prev, ok := reported[rec.Path]; ok && prev == sum {
		return nil
	}
	if fn != nil {
		if err := fn(rec); err != nil {
			return stripPath(err)
		}
	}
	reported[rec.Path] = sum
	return nil
}

// ReadAll captures every path through ReadBackup, tagging each record with meta.
func ReadAll(op string, workItemID int64, meta map[string]string, paths []string) ([]Backup, error) {
	recs := make([]Backup, 0, len(paths))
	for _, p := range paths {
		rec, err := ReadBackup(op, workItemID, p)
		if err != nil {
			return nil, err
		}
		rec.Meta = meta
		recs = append(recs, rec)
	}
	return recs, nil
}

// ReportAll reports each record not already reported with identical bytes.
func ReportAll(fn func(Backup) error, recs []Backup, reported reportedSet) error {
	for _, rec := range recs {
		if err := report(fn, rec, reported); err != nil {
			return err
		}
	}
	return nil
}
