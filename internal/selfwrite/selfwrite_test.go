package selfwrite

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRecordSuppressesExactPath(t *testing.T) {
	dir := t.TempDir()
	r := New(time.Minute)
	p := filepath.Join(dir, "song.lrc")
	if r.Suppress(p) {
		t.Fatal("Suppress before Record = true; want false")
	}
	r.Record(p)
	if !r.Suppress(p) {
		t.Error("Suppress after Record = false; want true")
	}
}

// TestSuppressLeavesOtherPathsAlone is the half of the contract that matters
// most: a filter that quiets the disk by suppressing everything has defeated the
// watcher entirely. A sibling file in the SAME directory must stay visible,
// which is why suppression is keyed on the file path and not its directory.
func TestSuppressLeavesOtherPathsAlone(t *testing.T) {
	dir := t.TempDir()
	r := New(time.Minute)
	r.Record(filepath.Join(dir, "song.lrc"))
	for _, other := range []string{"neighbor.lrc", "song.flac", "song.lrc.bak"} {
		if r.Suppress(filepath.Join(dir, other)) {
			t.Errorf("Suppress(%s) = true; want false (only the recorded path is self-generated)", other)
		}
	}
	if r.Suppress(filepath.Join(t.TempDir(), "song.lrc")) {
		t.Error("Suppress of the same base name in another directory = true; want false")
	}
}

// TestSuppressMatchesTempFileByDerivation covers the race the writer cannot
// otherwise close: os.CreateTemp picks the random component, so the temp file's
// Create event can reach the watcher before its name is known. Recording the
// FINAL path must therefore cover the temp file too.
func TestSuppressMatchesTempFileByDerivation(t *testing.T) {
	dir := t.TempDir()
	r := New(time.Minute)
	final := filepath.Join(dir, "song.lrc")
	r.Record(final)

	if !r.Suppress(final + ".3564910325" + TempExt) {
		t.Error("Suppress of the recorded path's temp file = false; want true")
	}
	// A temp file belonging to a DIFFERENT final path is still external.
	if r.Suppress(filepath.Join(dir, "other.lrc.99"+TempExt)) {
		t.Error("Suppress of an unrelated temp file = true; want false")
	}
	// A path merely ending in .tmp, with no random component, is not derived
	// from anything recorded.
	if r.Suppress(filepath.Join(dir, TempExt)) {
		t.Error("Suppress of a bare .tmp name = true; want false")
	}
}

func TestEntriesExpire(t *testing.T) {
	dir := t.TempDir()
	r := New(50 * time.Millisecond)
	now := time.Now()
	r.now = func() time.Time { return now }

	p := filepath.Join(dir, "song.lrc")
	r.Record(p)
	if !r.Suppress(p) {
		t.Fatal("Suppress inside the TTL = false; want true")
	}

	// Past the TTL the entry must be gone: a crash between the write and the
	// event, or an event the kernel never delivered, must not leave this path
	// permanently deaf to a genuine external change.
	now = now.Add(51 * time.Millisecond)
	if r.Suppress(p) {
		t.Error("Suppress past the TTL = true; want false (entries must expire)")
	}
	if n := r.Len(); n != 0 {
		t.Errorf("Len after expiry = %d; want 0 (expired entries must be pruned, not merely ignored)", n)
	}
}

// TestExpiryPrunesSoTheSetStaysBounded proves the map does not grow with the
// process lifetime: a set that never forgets is a worse bug than the one this
// package fixes.
func TestExpiryPrunesSoTheSetStaysBounded(t *testing.T) {
	dir := t.TempDir()
	r := New(time.Millisecond)
	now := time.Now()
	r.now = func() time.Time { return now }

	for i := 0; i < 200; i++ {
		r.Record(filepath.Join(dir, string(rune('a'+i%26))+".lrc"))
		now = now.Add(time.Millisecond)
	}
	if n := r.Len(); n > 1 {
		t.Errorf("Len after 200 expiring records = %d; want at most 1 live entry", n)
	}
}

func TestNilAndZeroTTLAreSafeNoOps(t *testing.T) {
	var nilReg *Registry
	nilReg.Record("/music/song.lrc") // must not panic
	if nilReg.Suppress("/music/song.lrc") {
		t.Error("nil registry Suppress = true; want false")
	}
	if nilReg.Len() != 0 {
		t.Error("nil registry Len != 0")
	}

	// A non-positive TTL degrades to suppressing nothing -- a noisy watcher,
	// never a deaf one.
	zero := New(0)
	zero.Record("/music/song.lrc")
	if zero.Suppress("/music/song.lrc") {
		t.Error("zero-TTL registry Suppress = true; want false (must degrade to no suppression)")
	}
}

func TestEmptyPathsAreIgnored(t *testing.T) {
	r := New(time.Minute)
	r.Record("")
	if r.Len() != 0 {
		t.Error("Record(\"\") stored an entry; want none")
	}
	if r.Suppress("") {
		t.Error("Suppress(\"\") = true; want false")
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	dir := t.TempDir()
	r := New(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := filepath.Join(dir, string(rune('a'+i))+".lrc")
			for j := 0; j < 100; j++ {
				r.Record(p)
				r.Suppress(p)
				r.Len()
			}
		}(i)
	}
	wg.Wait()
}

func TestTempPattern(t *testing.T) {
	if got := TempPattern("song.lrc"); got != "song.lrc.*"+TempExt {
		t.Errorf("TempPattern = %q; want the os.CreateTemp pattern trimTempSuffix reverses", got)
	}
}
