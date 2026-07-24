// Package pathutil provides path-containment checks used to confine filesystem
// targets to configured roots. It centralizes the containment logic that was
// previously duplicated across the server, watcher, and scan packages.
package pathutil

import (
	"path/filepath"
	"strings"
)

// WithinRoot reports whether p is root or sits under root using purely lexical
// analysis (filepath.Clean + filepath.Rel). It does NOT resolve symlinks, so a
// symlink inside root that points outside it still passes. It is the right check
// when the paths are already trusted or may not exist yet (matching filesystem
// event paths to their owning library, or comparing two configured roots). For
// an untrusted path that will be used as a real filesystem target, use
// ResolveWithinRoot instead.
func WithinRoot(root, p string) bool {
	// Fail closed on empty inputs so the helper is safe in isolation rather than
	// relying on callers to pre-filter: both "" clean to ".", which would
	// otherwise report a nonsensical empty path as contained.
	if root == "" || p == "" {
		return false
	}
	_, ok := relWithin(filepath.Clean(root), filepath.Clean(p))
	return ok
}

// ResolveWithinRoot reports whether p resolves to a location inside root and, on
// success, returns the fully resolved (symlink-free) path so callers can use the
// exact value they validated as the filesystem target (check path == write
// path).
//
// It derives the relative component of p against root and rejects any upward
// traversal, then rebuilds the candidate by joining the symlink-resolved root
// with that traversal-checked relative component. It resolves the result with
// filepath.EvalSymlinks and re-confirms containment, so the value handed back is
// anchored to the operator-configured root rather than to raw caller input, and
// a symlink that lives inside root but points outside it is rejected. Any
// resolve error (including a path that does not exist) yields ok=false, so
// callers fail closed.
//
// Note: this validates at check time only. A caller that later opens the
// returned path is still subject to a time-of-check/time-of-use race if an
// attacker can swap a component for a symlink in between; closing that fully
// requires resolving at open time (e.g. O_NOFOLLOW / openat) in the writing
// layer.
func ResolveWithinRoot(root, p string) (string, bool) {
	if root == "" || p == "" {
		return "", false
	}
	cleanRoot := filepath.Clean(root)
	rel, ok := relWithin(cleanRoot, filepath.Clean(p))
	if !ok {
		return "", false
	}
	resolvedRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		return "", false
	}
	// Rebuild from the trusted resolved root + the traversal-checked relative
	// component, then resolve symlinks and re-confine so an in-root symlink
	// cannot escape.
	resolved, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, rel))
	if err != nil {
		return "", false
	}
	if _, ok := relWithin(resolvedRoot, resolved); !ok {
		return "", false
	}
	return resolved, true
}

// relWithin returns the cleaned relative path from root to p and reports whether
// p is root itself or sits below it with no upward traversal. root and p are
// expected to already be cleaned.
func relWithin(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// CanonicalRoot resolves root once, for reuse across a whole library scan: it
// returns both an absolute (not-yet-symlink-resolved) form and a symlink-
// resolved canonical form. It never returns an error -- a scan must never fail
// on this -- so any resolve failure degrades canonRoot to the absolute form,
// which simply leaves that one root uncanonicalized rather than aborting the
// caller's real work.
//
// Paired with RebaseUnderCanonicalRoot, this lets a caller pay exactly ONE
// filepath.EvalSymlinks per scan for the whole tree under root, rather than one
// per file -- the property that makes the audio_durations cache (#441, #643)
// pay for itself on an array whose disks are kept spun down.
func CanonicalRoot(root string) (absRoot, canonRoot string) {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = filepath.Clean(root)
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		canon = abs
	}
	return abs, canon
}

// RebaseUnderCanonicalRoot rewrites p -- built from a walk rooted at absRoot,
// e.g. via repeated filepath.Join -- onto canonRoot, the symlink-resolved form
// CanonicalRoot returned for that same root, without re-resolving symlinks for
// p itself. This is the per-file half of the CanonicalRoot pair: the root's
// EvalSymlinks cost is paid once by the caller, and every file underneath is
// rebased with a pure string operation.
//
// p is expected to be rooted at absRoot. Anything else (a caller error, or a
// path that legitimately does not fall under absRoot) falls back to
// CanonicalPath -- a full EvalSymlinks for that one path -- so the result is
// still correct, just no longer cheap.
func RebaseUnderCanonicalRoot(absRoot, canonRoot, p string) string {
	rel, err := filepath.Rel(absRoot, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return CanonicalPath(p)
	}
	return filepath.Join(canonRoot, rel)
}

// CanonicalPath resolves path to an absolute, symlink-free form, best-effort: a
// resolve failure (a nonexistent path, a permission error) degrades to the
// absolute-but-unresolved form rather than an error, matching the "never fail
// the caller's real work" contract every audio_durations write path follows.
//
// Intended for a caller that processes one file at a time and can afford the
// EvalSymlinks cost per call -- e.g. the worker's fetch-time duration-cache
// write, where the file's own header read already paid for the disk access.
// A caller walking many files under one root should use CanonicalRoot plus
// RebaseUnderCanonicalRoot instead, to pay that cost once rather than per file.
func CanonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return canon
}
