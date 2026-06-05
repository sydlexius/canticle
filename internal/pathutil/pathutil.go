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
