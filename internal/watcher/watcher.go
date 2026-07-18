package watcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/rjeczalik/notify"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
)

// LibraryLister lists configured library roots.
type LibraryLister interface {
	List(ctx context.Context) ([]models.Library, error)
}

// ScanFunc performs a targeted scan of path on behalf of lib.
type ScanFunc func(ctx context.Context, lib models.Library, path string) error

// PruneFunc reconciles the database against the filesystem for a path that a
// Remove/Rename event reported as vanished. It does no rescan (disk-cheap: only
// a handful of os.Stat existence checks plus DB work, never a directory walk),
// so it needs no debounce. May be nil.
type PruneFunc func(ctx context.Context, path string) error

// Watcher watches configured library roots and triggers targeted scans.
type Watcher struct {
	cfg       Config
	libraries LibraryLister
	scan      ScanFunc
	prune     PruneFunc

	// armed, when non-nil, is invoked with a path each time its debounce timer is
	// set or reset in dispatch. It is a test-only synchronization seam (default
	// nil = no-op in production) that lets tests place a second event mid-window
	// deterministically -- observing the reset arming -- instead of sleeping.
	armed func(path string)
}

// New creates a Watcher. scan is invoked (after debouncing) with the owning
// library and the directory that changed. prune (may be nil) is invoked
// immediately, without debounce, for a Remove/Rename event whose path has
// vanished, so the durable rows for a deleted/moved source are reconciled
// reactively. Non-positive Debounce or MaxDirs are clamped to the package
// defaults: a zero Debounce would disable debouncing (scan on every raw event),
// and a non-positive MaxDirs would reject all roots.
func New(cfg Config, libraries LibraryLister, scan ScanFunc, prune PruneFunc) *Watcher {
	if cfg.Debounce <= 0 {
		cfg.Debounce = defaultDebounceMS * time.Millisecond
	}
	if cfg.MaxDirs <= 0 {
		cfg.MaxDirs = defaultMaxDirs
	}
	return &Watcher{cfg: cfg, libraries: libraries, scan: scan, prune: prune}
}

// libEvent is a debounced, library-resolved scan request.
type libEvent struct {
	lib  models.Library
	path string
}

// Run registers recursive watches for every configured library root and
// dispatches debounced scans until ctx is canceled. It fails fast if the watch
// budget would be exceeded rather than silently truncating coverage.
func (w *Watcher) Run(ctx context.Context) error {
	libs, err := w.libraries.List(ctx)
	if err != nil {
		return fmt.Errorf("watcher: list libraries: %w", err)
	}
	if len(libs) == 0 {
		slog.Warn("watcher: no libraries configured; nothing to watch")
		return nil
	}

	// Overlapping roots (e.g. /music and /music/classical) would otherwise be
	// counted and watched twice, inflating the dir count toward MaxDirs and
	// delivering duplicate events. Watch only the top-level roots; the full
	// libs slice is still used for ownership resolution in eventTarget.
	watched := dedupeRoots(libs)

	dirs, err := countDirs(watched)
	if err != nil {
		return err
	}
	if dirs > w.cfg.MaxDirs {
		return fmt.Errorf("watcher: %d directories under configured roots exceed %s=%d; raise the limit or narrow the roots", dirs, EnvMaxDirs, w.cfg.MaxDirs)
	}

	c := make(chan notify.EventInfo, eventBuffer)
	for _, lib := range watched {
		// "<root>/..." asks notify for a recursive watch over the subtree,
		// which also covers directories created after registration.
		if err := notify.Watch(filepath.Join(lib.Path, "..."), c, notify.Create, notify.Write, notify.Rename, notify.Remove); err != nil {
			notify.Stop(c)
			// An exhausted inotify quota arrives as ENOSPC ("no space left on
			// device"), which sends the reader to disk usage instead of a sysctl.
			// annotateWatchErr says what actually happened; other errors pass
			// through unchanged.
			return annotateWatchErr(fmt.Errorf("watcher: watch %s: %w", lib.Path, err), dirs)
		}
	}
	defer notify.Stop(c)
	slog.Info("watcher started", "libraries", len(watched), "directories", dirs, "debounce", w.cfg.Debounce)

	events := make(chan libEvent)
	go w.translate(ctx, c, libs, events)
	w.dispatch(ctx, events)
	return ctx.Err()
}

const eventBuffer = 1024

// translate maps raw filesystem events to library-resolved scan targets and
// forwards them until ctx is canceled.
func (w *Watcher) translate(ctx context.Context, c <-chan notify.EventInfo, libs []models.Library, out chan<- libEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ei := <-c:
			lib, dir, ok := eventTarget(libs, ei.Path())
			if !ok {
				continue
			}
			w.maybePrune(ctx, ei)
			slog.Debug("watcher: event received", "event", ei.Event().String(), "path", ei.Path(), "library", lib.ID, "dir", dir)
			select {
			case <-ctx.Done():
				return
			case out <- libEvent{lib: lib, path: dir}:
			}
		}
	}
}

// dispatch debounces incoming events with a per-directory quiet period and runs
// a scan once a directory has been idle for the debounce window. Each directory
// keeps its own timer, so a burst on one directory (a tagger rewriting an album)
// coalesces into a single scan without delaying scans of unrelated directories.
func (w *Watcher) dispatch(ctx context.Context, events <-chan libEvent) {
	type pending struct {
		lib   models.Library
		timer *time.Timer
	}
	timers := make(map[string]*pending)
	fired := make(chan string, eventBuffer)

	// Stop any in-flight timers when the loop exits so no goroutine outlives ctx.
	defer func() {
		for _, p := range timers {
			p.timer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			if p, ok := timers[ev.path]; ok {
				p.lib = ev.lib
				p.timer.Stop()
				p.timer.Reset(w.cfg.Debounce)
				if w.armed != nil {
					w.armed(ev.path)
				}
				continue
			}
			path := ev.path
			timers[path] = &pending{
				lib: ev.lib,
				timer: time.AfterFunc(w.cfg.Debounce, func() {
					select {
					case fired <- path:
					case <-ctx.Done():
					}
				}),
			}
			if w.armed != nil {
				w.armed(path)
			}
		case path := <-fired:
			p, ok := timers[path]
			if !ok {
				continue
			}
			delete(timers, path)
			slog.Debug("watcher: debounced scan triggered", "path", path, "library", p.lib.ID)
			if err := w.scan(ctx, p.lib, path); err != nil {
				slog.Warn("watcher scan failed", "path", path, "library", p.lib.ID, "error", err)
			}
		}
	}
}

// maybePrune runs the reactive database reconciliation for a Remove/Rename event
// whose path has actually vanished, in addition to the parent-directory rescan
// every event triggers. It is disk-cheap (a few os.Stat checks plus DB work, no
// rescan) and runs without debounce. Create/Write events never prune, and a Rename whose reported path is
// the NEW (still-present) name is skipped by the os.Stat guard. A nil PruneFunc
// disables reactive reconciliation (the periodic sweep remains the backstop).
func (w *Watcher) maybePrune(ctx context.Context, ei notify.EventInfo) {
	if w.prune == nil {
		return
	}
	if ei.Event()&(notify.Remove|notify.Rename) == 0 {
		return
	}
	if _, err := os.Stat(ei.Path()); !errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err := w.prune(ctx, ei.Path()); err != nil {
		slog.Warn("watcher: reactive prune failed; periodic sweep remains the backstop", "path", ei.Path(), "error", err)
	}
}

// eventTarget returns the library that owns path and the directory to scan. A
// file event scans the file's directory; a directory event scans that
// directory. When path no longer exists (delete/rename), its parent directory
// is rescanned to pick up sibling adds/changes. Deletions are reconciled
// separately: maybePrune deletes the vanished path's rows reactively on the same
// Remove/Rename event, and a lazy periodic sweep (see runServe) is the backstop.
// ok is false when no configured library contains path.
func eventTarget(libs []models.Library, path string) (models.Library, string, bool) {
	var best models.Library
	found := false
	for _, lib := range libs {
		if pathutil.WithinRoot(lib.Path, path) && (!found || len(lib.Path) > len(best.Path)) {
			best = lib
			found = true
		}
	}
	if !found {
		return models.Library{}, "", false
	}
	dir := path
	if info, err := os.Stat(path); err != nil {
		// A missing path is the expected case for delete/rename events; only
		// unexpected stat errors (permissions, I/O) are worth surfacing.
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("watcher: stat event path failed; scanning parent directory", "path", path, "error", err)
		}
		dir = filepath.Dir(path)
	} else if !info.IsDir() {
		dir = filepath.Dir(path)
	}
	// When path is the library root itself and no longer exists (a deleted or
	// renamed root), filepath.Dir walks above the root. Clamp back to the owning
	// library so a scan target never escapes the library it belongs to.
	if !pathutil.WithinRoot(best.Path, dir) {
		dir = best.Path
	}
	return best, dir, true
}

// dedupeRoots returns the libraries whose paths are not nested within another
// library's path, so overlapping roots (e.g. /music and /music/classical) are
// neither counted nor watched twice. Identical paths keep their first
// occurrence. The original libs slice is left intact for ownership resolution.
func dedupeRoots(libs []models.Library) []models.Library {
	kept := make([]models.Library, 0, len(libs))
	for i, lib := range libs {
		nested := false
		for j, other := range libs {
			if i == j {
				continue
			}
			// For identical paths, suppress only the later occurrence so exactly
			// one copy survives.
			if filepath.Clean(other.Path) == filepath.Clean(lib.Path) {
				if j < i {
					nested = true
					break
				}
				continue
			}
			if pathutil.WithinRoot(other.Path, lib.Path) {
				nested = true
				break
			}
		}
		if !nested {
			kept = append(kept, lib)
		}
	}
	return kept
}

// countDirs returns the number of directories under the library roots, used to
// enforce the watch budget before any watches are registered.
func countDirs(libs []models.Library) (int, error) {
	total := 0
	for _, lib := range libs {
		err := filepath.WalkDir(lib.Path, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				total++
			}
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("watcher: count directories under %s: %w", lib.Path, err)
		}
	}
	return total, nil
}
