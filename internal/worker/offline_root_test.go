package worker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// fakeHealer answers the library-root questions from fixed data and counts how
// often it is asked.
type fakeHealer struct {
	roots     []string
	online    map[string]bool
	probes    map[string]int
	rootCalls int
}

func newFakeHealer(online map[string]bool, roots ...string) *fakeHealer {
	return &fakeHealer{roots: roots, online: online, probes: map[string]int{}}
}

func (f *fakeHealer) LibraryRoots(context.Context) ([]string, error) {
	f.rootCalls++
	return f.roots, nil
}

func (f *fakeHealer) RootOnline(root string) bool {
	f.probes[root]++
	return f.online[root]
}

// clockedWorker is a fakeQueue worker whose clock the test moves by hand.
func clockedWorker(h OutputHealer) (*Worker, *time.Time) {
	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &scriptedWriter{})
	w.SetOutputHealer(h)
	clock := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return clock }
	return w, &clock
}

// An absent library root is a mount outage: no fetch, no attempt charged, the
// row set aside into the future, and the NEXT row in the queue is processed in
// the same pass. The pass is bounded so a row that is NOT parked into the future
// fails the test in seconds instead of re-dequeuing for the suite timeout.
func TestRun_OfflineRootParksRowAndContinues(t *testing.T) {
	rig := newHealRig(t, func(srcDir, _, _ string) []models.OutputPath {
		return []models.OutputPath{{Outdir: srcDir, Filename: "01.lrc"}}
	})
	dirB := filepath.Join(rig.rootB, "artist", "Other")
	nextID := rig.enqueue(t, "Two", dirB, filepath.Join(dirB, "01.flac"), nil)
	takeOffline(t, rig.root)
	// root is the offline one; rowB lives in rootB, which is online.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rig.w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("Run did not finish: the offline row was re-dequeued instead of being parked into the future")
	}
	status, attempts, next := rig.rowOf(t, rig.id)
	if status != "pending" || attempts != 0 {
		t.Errorf("offline row status=%s attempts=%d, want pending/0", status, attempts)
	}
	if ts, err := time.Parse(time.RFC3339, next); err != nil || !ts.After(time.Now().Add(time.Minute)) {
		t.Errorf("next_attempt_at = %q (%v), want it parked into the future", next, err)
	}
	if st, _, _ := rig.rowOf(t, nextID); st != "done" {
		t.Errorf("next row status = %s, want done in the same pass", st)
	}
	if rig.fetcher.calls != 1 {
		t.Errorf("fetches = %d, want 1 (only the online row)", rig.fetcher.calls)
	}
}

// The online answer and the library-root list are computed once per TTL, not
// once per row, so a spun-down array is not probed (nor the libraries table
// re-read) per row (#1430).
func TestRun_RootOnlineCheckedOncePerRoot(t *testing.T) {
	rig := newHealRig(t, func(srcDir, _, _ string) []models.OutputPath {
		return []models.OutputPath{{Outdir: srcDir, Filename: "01.lrc"}}
	})
	for _, name := range []string{"Two", "Three"} {
		dir := filepath.Join(rig.root, "artist", name)
		rig.enqueue(t, name, dir, mkSource(t, dir), nil)
	}
	if err := rig.w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rig.fetcher.calls != 3 {
		t.Fatalf("fetches = %d, want all 3 rows processed", rig.fetcher.calls)
	}
	if n := rig.healer.online[rig.root]; n != 1 {
		t.Errorf("RootOnline(root) called %d times for 3 rows, want 1", n)
	}
	if n := rig.healer.online[rig.rootB]; n != 0 {
		t.Errorf("RootOnline(rootB) called %d times, want 0: only the row's own root is checked", n)
	}
	if rig.healer.rootCalls != 1 {
		t.Errorf("LibraryRoots called %d times for 3 rows, want 1", rig.healer.rootCalls)
	}
}

// F8: an offline library of N rows costs ONE root lookup, ONE probe and ONE log
// line per TTL, not N of each; the line names no path.
func TestRun_OfflineLibraryCostsOneLookupAndOneLogLine(t *testing.T) {
	rig := newHealRig(t, func(srcDir, _, _ string) []models.OutputPath {
		return []models.OutputPath{{Outdir: srcDir, Filename: "01.lrc"}}
	})
	for _, name := range []string{"Two", "Three"} {
		dir := filepath.Join(rig.root, "artist", name)
		rig.enqueue(t, name, dir, filepath.Join(dir, "01.flac"), nil)
	}
	takeOffline(t, rig.root)
	recs := captureLogs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rig.w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rig.fetcher.calls != 0 {
		t.Fatalf("fetches = %d, want 0: every row is in the offline library", rig.fetcher.calls)
	}
	for _, id := range []int64{rig.id, rig.id + 1, rig.id + 2} {
		if st, a, _ := rig.rowOf(t, id); st != "pending" || a != 0 {
			t.Errorf("row %d status=%s attempts=%d, want pending/0", id, st, a)
		}
	}
	if rig.healer.rootCalls != 1 || rig.healer.online[rig.root] != 1 {
		t.Errorf("lookups=%d probes=%d for 3 parked rows, want 1 and 1", rig.healer.rootCalls, rig.healer.online[rig.root])
	}
	lines := 0
	for _, r := range *recs {
		if !strings.Contains(r.msg, "library root offline") {
			continue
		}
		lines++
		for _, v := range r.attrs {
			if strings.Contains(v.String(), rig.root) {
				t.Errorf("offline line carries a path: %v", r.attrs)
			}
		}
		if strings.Contains(r.msg, rig.root) {
			t.Errorf("offline line carries a path: %q", r.msg)
		}
	}
	if lines != 1 {
		t.Errorf("offline log lines = %d for 3 parked rows, want 1", lines)
	}
}

// The line fires once per root per TTL and reports the rows set aside since the
// last line; a root that comes back online re-arms it at once.
func TestNoteOffline_OneLinePerRootPerTTLWithCount(t *testing.T) {
	w, clock := clockedWorker(newFakeHealer(nil))
	recs := captureLogs(t)
	rowsOf := func() (lines []int64) {
		for _, r := range *recs {
			if strings.Contains(r.msg, "library root offline") {
				lines = append(lines, r.attrs["rows"].Int64())
			}
		}
		return lines
	}
	w.noteOffline("/a")
	w.noteOffline("/a")
	w.noteOffline("/a")
	w.noteOffline("/b") // a different root has its own line
	if got := rowsOf(); len(got) != 2 || got[0] != 1 || got[1] != 1 {
		t.Fatalf("lines (rows) = %v after 3 rows in /a and 1 in /b, want [1 1]", got)
	}
	*clock = clock.Add(rootOnlineTTL + time.Second)
	w.noteOffline("/a")
	if got := rowsOf(); len(got) != 3 || got[2] != 3 {
		t.Fatalf("lines (rows) = %v after the TTL, want a third line counting the 3 rows since the last", got)
	}
}

// F3b: the online answer is cached for the TTL and then re-asked, in BOTH
// directions: an unmounted share that returns is noticed, and so is a mount
// that drops.
func TestRootIsOnline_AnswerExpiresInBothDirections(t *testing.T) {
	fh := newFakeHealer(map[string]bool{"/r": false}, "/r")
	w, clock := clockedWorker(fh)
	if w.rootIsOnline("/r") {
		t.Fatal("offline root reported online")
	}
	fh.online["/r"] = true
	*clock = clock.Add(rootOnlineTTL - time.Second)
	if w.rootIsOnline("/r") {
		t.Fatal("answer must be reused inside the TTL")
	}
	*clock = clock.Add(2 * time.Second)
	if !w.rootIsOnline("/r") {
		t.Fatal("a root that came back online was not noticed after the TTL")
	}
	fh.online["/r"] = false
	*clock = clock.Add(rootOnlineTTL - time.Second)
	if !w.rootIsOnline("/r") {
		t.Fatal("answer must be reused inside the TTL")
	}
	*clock = clock.Add(2 * time.Second)
	if w.rootIsOnline("/r") {
		t.Fatal("a root that went offline was not noticed after the TTL")
	}
	if fh.probes["/r"] != 3 {
		t.Errorf("probes = %d, want 3 (one per TTL window)", fh.probes["/r"])
	}
}

// The cached library-root list expires too, so a library added or removed is
// seen without a restart.
func TestLibraryRoots_ListExpires(t *testing.T) {
	fh := newFakeHealer(nil, "/a")
	w, clock := clockedWorker(fh)
	ctx := context.Background()
	if got, _ := w.libraryRoots(ctx); len(got) != 1 {
		t.Fatalf("roots = %v, want [/a]", got)
	}
	fh.roots = []string{"/a", "/b"}
	*clock = clock.Add(rootOnlineTTL - time.Second)
	if got, _ := w.libraryRoots(ctx); len(got) != 1 || fh.rootCalls != 1 {
		t.Fatalf("roots = %v calls=%d, want the cached list inside the TTL", got, fh.rootCalls)
	}
	*clock = clock.Add(2 * time.Second)
	if got, _ := w.libraryRoots(ctx); len(got) != 2 || fh.rootCalls != 2 {
		t.Fatalf("roots = %v calls=%d, want the list re-read after the TTL", got, fh.rootCalls)
	}
}
