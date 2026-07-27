// Package selfwrite holds a short-lived record of the filesystem paths this
// process just wrote, so the filesystem watcher can tell canticle's own sidecar
// writes apart from a genuine third-party change (#685).
//
// It is a leaf package on purpose. The lyrics writer records into it and the
// watcher reads from it; neither imports the other, and the naming convention
// shared between them (the writer's temp-file pattern) lives here, in the one
// place both sides already depend on.
//
// Every entry EXPIRES. A crash between Record and the event that would have
// consumed it, or an event the kernel never delivered, must not leave a path
// permanently deaf to external change, so suppression is a short time window
// (a small multiple of the watcher's debounce) rather than a one-shot token
// waiting to be matched.
package selfwrite

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/canticle/internal/pathutil"
)

// TempExt is the extension the lyrics writer's atomic-write temp file carries.
const TempExt = ".tmp"

// TempPattern returns the os.CreateTemp pattern for an atomic write to name.
// The resulting temp file is always "<name>.<random>.tmp", which is what
// trimTempSuffix reverses.
func TempPattern(name string) string {
	return name + ".*" + TempExt
}

// Registry records recently written paths and answers whether a filesystem
// event for a path should be treated as self-generated. It is safe for
// concurrent use, and a nil *Registry is a valid no-op: callers outside serve
// mode (the fetch CLI, tests) construct no registry and are unaffected.
type Registry struct {
	ttl time.Duration

	mu      sync.Mutex
	expires map[string]time.Time

	// now is the clock, swappable in tests so expiry is exercised without
	// sleeping through a real TTL.
	now func() time.Time
}

// New returns a Registry whose entries expire after ttl. A non-positive ttl
// yields a registry that suppresses nothing, so a misconfigured caller degrades
// to today's behavior (a noisy watcher) rather than to a deaf one.
func New(ttl time.Duration) *Registry {
	return &Registry{ttl: ttl, expires: make(map[string]time.Time), now: time.Now}
}

// Record marks each path as written by this process, suppressing events for it
// until the TTL elapses. Paths are canonicalized so a recorded path and the
// path a filesystem event reports for the same file compare equal.
func (r *Registry) Record(paths ...string) {
	if r == nil || r.ttl <= 0 {
		return
	}
	deadline := r.now().Add(r.ttl)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range paths {
		if p == "" {
			continue
		}
		r.expires[canonical(p)] = deadline
	}
	r.pruneLocked()
}

// Suppress reports whether an event for path should be dropped as
// self-generated, and prunes expired entries as it goes.
//
// It matches an exact recorded path, and it also matches the temp file of a
// recorded path ("<recorded>.<random>.tmp"). The temp match is what closes the
// unavoidable race in the writer: os.CreateTemp picks the random component, so
// the Create event for the temp file can reach the watcher before the writer
// has had a chance to record the resulting name. Recording the FINAL path
// before the write begins covers the temp file by derivation, with no window.
func (r *Registry) Suppress(path string) bool {
	if r == nil || r.ttl <= 0 || path == "" {
		return false
	}
	c := canonical(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	if _, ok := r.expires[c]; ok {
		return true
	}
	if base, ok := trimTempSuffix(c); ok {
		if _, ok := r.expires[base]; ok {
			return true
		}
	}
	return false
}

// Len returns the number of live entries. Exported for tests and diagnostics so
// unbounded growth is observable rather than inferred.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	return len(r.expires)
}

// pruneLocked drops expired entries. Called on every operation, so the map is
// bounded by the number of distinct paths written within one TTL rather than by
// the process lifetime.
func (r *Registry) pruneLocked() {
	now := r.now()
	for p, deadline := range r.expires {
		if !now.Before(deadline) {
			delete(r.expires, p)
		}
	}
}

// canonical resolves p to a comparable form. Only the DIRECTORY is symlink
// resolved: the file itself routinely does not exist on either side (the writer
// records a final path before creating it, and an event reports a path that was
// just removed), and pathutil.CanonicalPath degrades to the unresolved absolute
// form for a missing path -- which would leave "/tmp/x/a.lrc" recorded against
// "/private/tmp/x/a.lrc" reported. The containing directory always exists for
// both sides, so resolving that and rejoining the base name compares equal.
func canonical(p string) string {
	dir, base := filepath.Split(filepath.Clean(p))
	if dir == "" {
		return filepath.Clean(p)
	}
	return filepath.Join(pathutil.CanonicalPath(filepath.Clean(dir)), base)
}

// trimTempSuffix reverses TempPattern: it strips a "<random>.tmp" suffix and
// returns the path the temp file was created for.
func trimTempSuffix(p string) (string, bool) {
	if !strings.HasSuffix(p, TempExt) {
		return "", false
	}
	rest := strings.TrimSuffix(p, TempExt)
	i := strings.LastIndex(rest, ".")
	if i <= 0 {
		return "", false
	}
	return rest[:i], true
}
