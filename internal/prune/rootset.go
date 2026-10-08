package prune

import (
	"path/filepath"

	"github.com/sydlexius/canticle/internal/pathutil"
)

// rootSet matches paths to configured library roots under either spelling of a
// root (#1430). Webhook rows carry the symlink-resolved path while the roots are
// stored as configured, so a path under a symlinked root must match through the
// resolved alias too; the worker's offline-root guard does the same.
//
// Configured -> resolved is a function even when several configured roots share
// a target; resolved -> configured is not. So comparison is done in the RESOLVED
// spelling (toResolved) and the configured root is kept only as the key handed to
// a caller's online check (rootOf). Any ambiguity answers "no match", which every
// caller reads as "not proven".
type rootSet struct {
	spellings  []string            // configured roots plus their resolved aliases
	owners     map[string][]string // spelling -> distinct configured roots using it
	target     map[string]string   // spelling -> the resolved root it stands for
	conflict   map[string]bool     // spelling that maps to two different resolved roots
	tangled    map[string]bool     // alias equal to or inside a DIFFERENT configured root
	unresolved map[string]bool     // configured roots whose symlink did not resolve
}

// newRootSet resolves each root's own symlink. A root that does not resolve has
// no alias and is recorded in unresolved: a resolved-spelling path then matches
// nothing, and provablyStale refuses any directory under it.
func newRootSet(roots []string) rootSet {
	s := rootSet{
		spellings: make([]string, 0, 2*len(roots)),
		owners:    map[string][]string{}, target: map[string]string{},
		conflict: map[string]bool{}, tangled: map[string]bool{}, unresolved: map[string]bool{},
	}
	add := func(spelling, owner, target string) {
		spelling = filepath.Clean(spelling)
		if prev, seen := s.target[spelling]; seen && prev != target {
			s.conflict[spelling] = true
		}
		if !seen(s.owners[spelling], owner) {
			s.owners[spelling] = append(s.owners[spelling], owner)
		}
		if _, dup := s.target[spelling]; !dup {
			s.spellings = append(s.spellings, spelling)
		}
		s.target[spelling] = target
	}
	for _, r := range roots {
		c := filepath.Clean(r)
		resolved, err := filepath.EvalSymlinks(c)
		if err != nil {
			s.unresolved[c] = true
			add(c, c, c)
			continue
		}
		add(c, c, resolved)
		if resolved != c {
			add(resolved, c, resolved)
		}
	}
	for spelling, own := range s.owners {
		for _, other := range roots {
			o := filepath.Clean(other)
			if !seen(own, o) && pathutil.WithinRoot(o, spelling) {
				s.tangled[spelling] = true
			}
		}
	}
	return s
}

func seen(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// match returns the longest spelling containing p, or ok=false when none does or
// the match is ambiguous about which resolved root it names.
func (s rootSet) match(p string) (spelling string, ok bool) {
	spelling, ok = pathutil.ContainingRoot(s.spellings, p)
	if !ok || s.conflict[spelling] || s.tangled[spelling] {
		return "", false
	}
	return spelling, true
}

// rootOf returns the single configured root containing p under either spelling.
// A spelling shared by several configured roots names no single one: not found.
func (s rootSet) rootOf(p string) (string, bool) {
	spelling, ok := s.match(p)
	if !ok || len(s.owners[spelling]) != 1 {
		return "", false
	}
	return s.owners[spelling][0], true
}

// toResolved rewrites p into the resolved spelling of its root, so two paths to
// the same place compare equal whichever spelling they carry. ok is false when p
// lies under no root or the root match is ambiguous.
func (s rootSet) toResolved(p string) (string, bool) {
	spelling, ok := s.match(p)
	if !ok {
		return "", false
	}
	rel, err := filepath.Rel(spelling, p)
	if err != nil {
		return "", false
	}
	return filepath.Join(s.target[spelling], rel), true
}
