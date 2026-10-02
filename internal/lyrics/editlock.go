package lyrics

import (
	"path/filepath"
	"sync"
)

// editPathLocks is the process-wide per-sidecar exclusion between a hand edit
// and an automatic remediation of the same .lrc (#1226).
//
// THE PROTOCOL. The hand-edit mark (work_queue.lyric_edited_at) is what keeps
// automatic paths away from an edited .lrc, but a mark read once and acted on
// later is a race: the serve-mode timing sweep reads the marks of its batch,
// then moves files, and an editor save that lands in between would have its
// fresh edit demoted or quarantined. So both sides take this lock for the
// sidecar path:
//
//   - The editor (web save and revert) holds it from before it sets the mark
//     until after ApplyEdit (and a revert's mark clear) returns, so the mark
//     and the file it describes change together as seen by a lock holder.
//   - The sweep, for each planned remediation move, takes it, RE-READS the
//     mark of every row owning that sidecar, and applies the move only when
//     none is marked, all before unlocking.
//
// Either order is then safe: an edit that wins the lock is complete (mark set,
// file written) before the sweep's re-check, which therefore skips the move;
// a sweep that wins moves the file before the editor writes, so ApplyEdit's
// mtime check refuses the save rather than recreating an edit beside the
// remediated file. The `revalidate --apply` CLI is an operator override and
// does not take the lock.
//
// It is IN-PROCESS only: both writers live in the one serve process. Entries
// are refcounted and dropped on the last release, so the map holds only paths
// with a holder or a waiter, never every path ever edited.
var editPathLocks = struct {
	mu sync.Mutex
	m  map[string]*editPathLock
}{m: map[string]*editPathLock{}}

type editPathLock struct {
	sync.Mutex
	refs int
}

// editLockKey is the map key for path: absolute and cleaned, so "a/../x.lrc"
// and "x.lrc" from the same cwd contend. A path that cannot be made absolute
// is keyed cleaned as given (both callers pass absolute library paths).
func editLockKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// LockEditPath takes the per-sidecar edit lock for path and returns its
// release. See editPathLocks for the protocol. The release must be called
// exactly once.
func LockEditPath(path string) (unlock func()) {
	key := editLockKey(path)
	editPathLocks.mu.Lock()
	e := editPathLocks.m[key]
	if e == nil {
		e = &editPathLock{}
		editPathLocks.m[key] = e
	}
	e.refs++
	editPathLocks.mu.Unlock()
	e.Lock()
	return func() {
		e.Unlock()
		editPathLocks.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(editPathLocks.m, key)
		}
		editPathLocks.mu.Unlock()
	}
}
