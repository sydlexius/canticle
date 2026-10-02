package lyrics

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func editLockEntries() int {
	editPathLocks.mu.Lock()
	defer editPathLocks.mu.Unlock()
	return len(editPathLocks.m)
}

// TestLockEditPathExcludesAndEmptiesTheMap (#1226): a second holder of the same
// path (spelled differently) waits for the first, and once every holder has
// released, the map is empty again.
func TestLockEditPathExcludesAndEmptiesTheMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "track.lrc")
	unlock := LockEditPath(path)

	acquired := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Raw concatenation: filepath.Join would clean the ".." itself.
		u := LockEditPath(dir + string(filepath.Separator) + "sub" + string(filepath.Separator) + ".." + string(filepath.Separator) + "track.lrc")
		close(acquired)
		u()
	}()
	select {
	case <-acquired:
		t.Fatal("a second holder acquired the lock while the first still held it")
	case <-time.After(100 * time.Millisecond):
	}
	if n := editLockEntries(); n != 1 {
		t.Errorf("entries while contended = %d; want 1 (one key, refcounted)", n)
	}
	unlock()
	wg.Wait()
	if n := editLockEntries(); n != 0 {
		t.Errorf("entries after every release = %d; want 0", n)
	}

	// A different path never contends.
	u1 := LockEditPath(path)
	u2 := LockEditPath(filepath.Join(dir, "other.lrc"))
	u2()
	u1()
	if n := editLockEntries(); n != 0 {
		t.Errorf("entries after independent locks = %d; want 0", n)
	}
}
