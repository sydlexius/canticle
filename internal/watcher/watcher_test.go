package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rjeczalik/notify"

	"github.com/sydlexius/canticle/internal/models"
)

// waitFor polls cond until it returns true or a generous deadline elapses,
// failing the test on timeout. It is the deterministic replacement for a fixed
// "sleep then assert" wait: the test proceeds the instant the observed effect
// occurs, so the effect's code path is always exercised regardless of machine
// load (which a fixed sleep cannot guarantee, causing intermittent coverage
// flap). Mirrors the helper in internal/orchestrator/parallel_test.go.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

type fakeLister struct {
	libs []models.Library
	err  error
}

func (f fakeLister) List(context.Context) ([]models.Library, error) {
	return f.libs, f.err
}

func TestConfigFromEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv(EnvEnabled, "")
		t.Setenv(EnvDebounceMS, "")
		t.Setenv(EnvMaxDirs, "")
		cfg := ConfigFromEnv()
		if cfg.Enabled {
			t.Error("Enabled = true; want false by default")
		}
		if cfg.Debounce != defaultDebounceMS*time.Millisecond {
			t.Errorf("Debounce = %s; want %dms", cfg.Debounce, defaultDebounceMS)
		}
		if cfg.MaxDirs != defaultMaxDirs {
			t.Errorf("MaxDirs = %d; want %d", cfg.MaxDirs, defaultMaxDirs)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv(EnvEnabled, "true")
		t.Setenv(EnvDebounceMS, "500")
		t.Setenv(EnvMaxDirs, "42")
		cfg := ConfigFromEnv()
		if !cfg.Enabled {
			t.Error("Enabled = false; want true")
		}
		if cfg.Debounce != 500*time.Millisecond {
			t.Errorf("Debounce = %s; want 500ms", cfg.Debounce)
		}
		if cfg.MaxDirs != 42 {
			t.Errorf("MaxDirs = %d; want 42", cfg.MaxDirs)
		}
	})

	t.Run("invalid falls back", func(t *testing.T) {
		t.Setenv(EnvDebounceMS, "notanumber")
		t.Setenv(EnvMaxDirs, "-5")
		cfg := ConfigFromEnv()
		if cfg.Debounce != defaultDebounceMS*time.Millisecond {
			t.Errorf("Debounce = %s; want default after invalid", cfg.Debounce)
		}
		if cfg.MaxDirs != defaultMaxDirs {
			t.Errorf("MaxDirs = %d; want default after invalid", cfg.MaxDirs)
		}
	})
}

func TestEventTargetResolvesOwningLibrary(t *testing.T) {
	libs := []models.Library{
		{ID: 1, Path: "/music"},
		{ID: 2, Path: "/music/classical"}, // nested, more specific
	}

	// A file under the nested library resolves to the most specific root, and
	// the scan target is the file's directory.
	lib, dir, ok := eventTarget(libs, "/music/classical/Bach/aria.flac")
	if !ok {
		t.Fatal("eventTarget ok = false; want true")
	}
	if lib.ID != 2 {
		t.Errorf("lib ID = %d; want 2 (most specific root)", lib.ID)
	}
	if dir != "/music/classical/Bach" {
		t.Errorf("dir = %q; want the file's directory", dir)
	}

	// A path outside every library is not a target.
	if _, _, ok := eventTarget(libs, "/somewhere/else/x.mp3"); ok {
		t.Error("eventTarget for outside path ok = true; want false")
	}
}

func TestEventTargetClampsDeletedRootToLibrary(t *testing.T) {
	// The library root itself no longer exists (deleted/renamed). filepath.Dir
	// would walk above the root, so eventTarget must clamp the scan target back
	// to the owning library rather than scanning its parent.
	libs := []models.Library{{ID: 1, Path: "/music/library"}}
	lib, dir, ok := eventTarget(libs, "/music/library")
	if !ok {
		t.Fatal("eventTarget ok = false; want true")
	}
	if lib.ID != 1 {
		t.Errorf("lib ID = %d; want 1", lib.ID)
	}
	if dir != "/music/library" {
		t.Errorf("dir = %q; want the library root (clamped, not its parent)", dir)
	}
}

func TestDedupeRoots(t *testing.T) {
	t.Run("drops nested roots", func(t *testing.T) {
		libs := []models.Library{
			{ID: 1, Path: "/music"},
			{ID: 2, Path: "/music/classical"}, // nested under /music
			{ID: 3, Path: "/other"},
		}
		got := dedupeRoots(libs)
		if len(got) != 2 {
			t.Fatalf("dedupeRoots len = %d; want 2", len(got))
		}
		ids := map[int64]bool{got[0].ID: true, got[1].ID: true}
		if !ids[1] || !ids[3] || ids[2] {
			t.Errorf("kept IDs = %v; want top-level roots 1 and 3 only", ids)
		}
	})

	t.Run("keeps one of identical paths", func(t *testing.T) {
		libs := []models.Library{
			{ID: 1, Path: "/music"},
			{ID: 2, Path: "/music"},
		}
		got := dedupeRoots(libs)
		if len(got) != 1 || got[0].ID != 1 {
			t.Errorf("dedupeRoots = %+v; want a single entry keeping the first occurrence", got)
		}
	})
}

func TestEventTargetClampsDeletedNestedRoot(t *testing.T) {
	libs := []models.Library{
		{ID: 1, Path: "/music"},
		{ID: 2, Path: "/music/classical"},
	}
	// The nested root itself is the event path and no longer exists. The target
	// must clamp to the nested root (ID 2), not walk up into the broader /music
	// library, which would scan far more than the event warranted.
	lib, dir, ok := eventTarget(libs, "/music/classical")
	if !ok {
		t.Fatal("eventTarget ok = false; want true")
	}
	if lib.ID != 2 {
		t.Errorf("lib ID = %d; want 2 (most specific root)", lib.ID)
	}
	if dir != "/music/classical" {
		t.Errorf("dir = %q; want /music/classical (clamped, not parent /music)", dir)
	}
}

func TestCountDirs(t *testing.T) {
	root := t.TempDir()
	for _, sub := range []string{"a", "a/b", "c"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	n, err := countDirs([]models.Library{{Path: root}})
	if err != nil {
		t.Fatalf("countDirs: %v", err)
	}
	// root + a + a/b + c = 4
	if n != 4 {
		t.Errorf("countDirs = %d; want 4", n)
	}
}

func TestDispatchCoalescesBurstIntoSingleScan(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	scan := func(_ context.Context, _ models.Library, path string) error {
		mu.Lock()
		calls[path]++
		mu.Unlock()
		return nil
	}
	w := New(Config{Debounce: 30 * time.Millisecond}, nil, scan, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan libEvent)
	done := make(chan struct{})
	go func() { w.dispatch(ctx, events); close(done) }()

	lib := models.Library{ID: 1, Path: "/m"}
	for i := 0; i < 5; i++ { // burst on one dir
		events <- libEvent{lib: lib, path: "/m/Album"}
	}
	events <- libEvent{lib: lib, path: "/m/Other"} // a second dir

	// Wait deterministically for both dirs to flush, rather than a fixed sleep
	// that under load may elapse before the flush goroutine fires.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls["/m/Album"] == 1 && calls["/m/Other"] == 1
	}, "burst to coalesce into one scan per dir")

	mu.Lock()
	album, other := calls["/m/Album"], calls["/m/Other"]
	mu.Unlock()
	if album != 1 {
		t.Errorf("scans for /m/Album = %d; want 1 (burst coalesced)", album)
	}
	if other != 1 {
		t.Errorf("scans for /m/Other = %d; want 1", other)
	}

	cancel()
	<-done
}

func TestDispatchTrailingEdgeResetsTimer(t *testing.T) {
	var mu sync.Mutex
	scans := 0
	var scanAt, lastArmAt time.Time
	scan := func(_ context.Context, _ models.Library, _ string) error {
		mu.Lock()
		scans++
		scanAt = time.Now()
		mu.Unlock()
		return nil
	}
	const debounce = 50 * time.Millisecond
	w := New(Config{Debounce: debounce}, nil, scan, nil)
	armed := make(chan string, 4)
	// The callback runs synchronously inside dispatch immediately after the timer
	// is set/reset, so lastArmAt tracks the real dispatch-side arm time (not a
	// jittery test-goroutine timestamp).
	w.armed = func(p string) {
		mu.Lock()
		lastArmAt = time.Now()
		mu.Unlock()
		armed <- p
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan libEvent)
	done := make(chan struct{})
	go func() { w.dispatch(ctx, events); close(done) }()

	lib := models.Library{ID: 1, Path: "/m"}
	// The first event arms the timer; the second event for the same path RESETS
	// it. Gate the second send on observing the first arming, and prove the reset
	// by observing a SECOND arming for the same path (only dispatch's reset branch
	// emits it) -- fully deterministic, no mid-window sleep.
	events <- libEvent{lib: lib, path: "/m/Album"}
	if p := <-armed; p != "/m/Album" {
		t.Fatalf("first arm = %q; want /m/Album", p)
	}
	events <- libEvent{lib: lib, path: "/m/Album"}
	if p := <-armed; p != "/m/Album" {
		t.Fatalf("reset arm = %q; want /m/Album (second event must reset the timer)", p)
	}
	mu.Lock()
	secondArmAt := lastArmAt
	mu.Unlock()

	// Exactly one scan flushes: the two events coalesced into a single
	// trailing-edge scan.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return scans == 1
	}, "exactly one scan after the reset window")

	mu.Lock()
	final, at := scans, scanAt
	mu.Unlock()
	if final != 1 {
		t.Errorf("scans after the reset window = %d; want exactly 1 (trailing-edge coalesced)", final)
	}
	// Trailing-edge timing: the scan must fire no earlier than the RESET arm plus
	// the full debounce. A timer fires exactly debounce after Reset (captured at
	// secondArmAt inside dispatch), so a correctly reset timer always satisfies
	// this and it can never false-positive; a timer firing earlier means the
	// debounce was not honored from the trailing (second) event.
	if at.Before(secondArmAt.Add(debounce)) {
		t.Errorf("scan fired %s before secondArm+debounce; debounce not honored from the trailing edge",
			secondArmAt.Add(debounce).Sub(at))
	}

	cancel()
	<-done
}

func TestDispatchNoScanAfterCancelMidDebounce(t *testing.T) {
	var mu sync.Mutex
	scans := 0
	scan := func(_ context.Context, _ models.Library, _ string) error {
		mu.Lock()
		scans++
		mu.Unlock()
		return nil
	}
	w := New(Config{Debounce: 50 * time.Millisecond}, nil, scan, nil)
	ctx, cancel := context.WithCancel(context.Background())

	events := make(chan libEvent)
	done := make(chan struct{})
	go func() { w.dispatch(ctx, events); close(done) }()

	events <- libEvent{lib: models.Library{ID: 1}, path: "/m/Album"} // arms a debounce timer
	cancel()                                                         // cancel before the window elapses
	<-done                                                           // dispatch returns and its deferred Stop runs

	// No further wait needed: <-done already guarantees dispatch returned and its
	// deferred timer.Stop ran, so a pending debounce timer can no longer fire.
	mu.Lock()
	got := scans
	mu.Unlock()
	if got != 0 {
		t.Errorf("scans after cancel mid-debounce = %d; want 0 (pending timer must not fire a scan)", got)
	}
}

func TestRunReturnsNilWhenNoLibraries(t *testing.T) {
	w := New(Config{MaxDirs: defaultMaxDirs}, fakeLister{}, func(context.Context, models.Library, string) error { return nil }, nil)
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run with no libraries = %v; want nil", err)
	}
}

func TestRunFailsWhenWatchBudgetExceeded(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// root + sub = 2 directories, over the MaxDirs=1 cap. (MaxDirs must be
	// positive now; New clamps <= 0 to the default, so 0 no longer forces this.)
	w := New(Config{MaxDirs: 1, Debounce: time.Millisecond}, fakeLister{libs: []models.Library{{ID: 1, Path: root}}},
		func(context.Context, models.Library, string) error { return nil }, nil)
	err := w.Run(context.Background())
	if err == nil {
		t.Fatal("Run with exceeded budget = nil; want a loud failure")
	}
}

func TestCountDirsErrorsOnMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := countDirs([]models.Library{{ID: 1, Path: missing}}); err == nil {
		t.Fatal("countDirs on a missing root = nil error; want failure")
	}
}

// TestRunTriggersScanOnFileCreate exercises the real notify integration: a file
// created under a watched root must trigger a scan within the debounce window.
// Filesystem event delivery is best-effort and platform dependent, so the test
// allows a generous timeout.
func TestRunTriggersScanOnFileCreate(t *testing.T) {
	root := t.TempDir()
	scanned := make(chan string, 1)
	w := New(
		Config{Debounce: 20 * time.Millisecond, MaxDirs: defaultMaxDirs},
		fakeLister{libs: []models.Library{{ID: 5, Path: root}}},
		func(_ context.Context, _ models.Library, path string) error {
			select {
			case scanned <- path:
			default:
			}
			return nil
		},
		nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- w.Run(ctx) }()

	time.Sleep(200 * time.Millisecond) // allow watch registration
	if err := os.WriteFile(filepath.Join(root, "new.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	select {
	case got := <-scanned:
		if got != root {
			t.Errorf("scanned path = %q; want %q", got, root)
		}
	case <-time.After(5 * time.Second):
		t.Skip("no filesystem event delivered within 5s (best-effort watcher; may be unsupported here)")
	}

	cancel()
	<-runErr
}

// fakeEventInfo is a stand-in notify.EventInfo for direct maybePrune tests, so
// the prune-dispatch logic is covered without depending on real filesystem event
// delivery (which some CI filesystems do not support).
type fakeEventInfo struct {
	ev   notify.Event
	path string
}

func (f fakeEventInfo) Event() notify.Event { return f.ev }
func (f fakeEventInfo) Path() string        { return f.path }
func (f fakeEventInfo) Sys() any            { return nil }

// TestMaybePrune covers the reactive-prune dispatch decision table directly.
func TestMaybePrune(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.mp3")
	if err := os.WriteFile(present, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gone := filepath.Join(dir, "gone.mp3")

	cases := []struct {
		name     string
		ev       notify.Event
		path     string
		nilPrune bool
		want     bool
	}{
		{"remove-gone-prunes", notify.Remove, gone, false, true},
		{"rename-gone-prunes", notify.Rename, gone, false, true},
		{"create-never-prunes", notify.Create, gone, false, false},
		{"write-never-prunes", notify.Write, gone, false, false},
		{"remove-but-present-skips", notify.Remove, present, false, false},
		{"nil-prune-is-noop", notify.Remove, gone, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var pf PruneFunc
			if !tc.nilPrune {
				pf = func(_ context.Context, p string) error { got = p; return nil }
			}
			w := New(Config{}, nil, func(context.Context, models.Library, string) error { return nil }, pf)
			w.maybePrune(context.Background(), fakeEventInfo{ev: tc.ev, path: tc.path})
			if tc.want && got != tc.path {
				t.Errorf("prune called with %q; want %q", got, tc.path)
			}
			if !tc.want && got != "" {
				t.Errorf("prune called (%q) when it should not have been", got)
			}
		})
	}
}

// TestRunPrunesOnRemove verifies a Remove event for a vanished path invokes the
// injected PruneFunc with that path, while a Create does not.
func TestRunPrunesOnRemove(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "gone.mp3")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	pruned := make(chan string, 4)
	w := New(
		Config{Debounce: 20 * time.Millisecond, MaxDirs: defaultMaxDirs},
		fakeLister{libs: []models.Library{{ID: 7, Path: root}}},
		func(context.Context, models.Library, string) error { return nil },
		func(_ context.Context, path string) error {
			select {
			case pruned <- path:
			default:
			}
			return nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- w.Run(ctx) }()

	time.Sleep(200 * time.Millisecond) // allow watch registration
	// A create must not prune (only Remove/Rename of a vanished path does).
	if err := os.WriteFile(filepath.Join(root, "created.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write created: %v", err)
	}
	// Remove the pre-existing file: its path is now gone, so it must prune.
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove target: %v", err)
	}

	select {
	case got := <-pruned:
		if got != target {
			t.Errorf("pruned path = %q; want %q (a create must not prune)", got, target)
		}
	case <-time.After(5 * time.Second):
		t.Skip("no filesystem event delivered within 5s (best-effort watcher; may be unsupported here)")
	}

	cancel()
	<-runErr
}
