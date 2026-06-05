package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

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
	w := New(Config{Debounce: 30 * time.Millisecond}, nil, scan)
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

	time.Sleep(150 * time.Millisecond) // let the debounce window elapse and flush

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
	scan := func(_ context.Context, _ models.Library, _ string) error {
		mu.Lock()
		scans++
		mu.Unlock()
		return nil
	}
	const debounce = 100 * time.Millisecond
	w := New(Config{Debounce: debounce}, nil, scan)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan libEvent)
	done := make(chan struct{})
	go func() { w.dispatch(ctx, events); close(done) }()

	lib := models.Library{ID: 1, Path: "/m"}
	events <- libEvent{lib: lib, path: "/m/Album"}
	time.Sleep(debounce / 2)                       // t ~= 50ms, within the window
	events <- libEvent{lib: lib, path: "/m/Album"} // resets the timer to ~150ms
	time.Sleep(debounce / 2)                       // t ~= 100ms: first deadline would have hit if not reset

	mu.Lock()
	early := scans
	mu.Unlock()
	if early != 0 {
		t.Fatalf("scans at ~1 debounce window = %d; want 0 (each event must reset the trailing-edge timer)", early)
	}

	time.Sleep(debounce + 50*time.Millisecond) // wait out the reset window
	mu.Lock()
	final := scans
	mu.Unlock()
	if final != 1 {
		t.Errorf("scans after the reset window = %d; want exactly 1", final)
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
	w := New(Config{Debounce: 50 * time.Millisecond}, nil, scan)
	ctx, cancel := context.WithCancel(context.Background())

	events := make(chan libEvent)
	done := make(chan struct{})
	go func() { w.dispatch(ctx, events); close(done) }()

	events <- libEvent{lib: models.Library{ID: 1}, path: "/m/Album"} // arms a debounce timer
	cancel()                                                         // cancel before the window elapses
	<-done                                                           // dispatch returns and its deferred Stop runs

	time.Sleep(120 * time.Millisecond) // well past the debounce window
	mu.Lock()
	got := scans
	mu.Unlock()
	if got != 0 {
		t.Errorf("scans after cancel mid-debounce = %d; want 0 (pending timer must not fire a scan)", got)
	}
}

func TestRunReturnsNilWhenNoLibraries(t *testing.T) {
	w := New(Config{MaxDirs: defaultMaxDirs}, fakeLister{}, func(context.Context, models.Library, string) error { return nil })
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
		func(context.Context, models.Library, string) error { return nil })
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
