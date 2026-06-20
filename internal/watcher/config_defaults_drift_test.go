package watcher

import (
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
)

// TestWatcherDefaultsMatchConfigPackage guards the duplicated defaults: the
// central config's [watcher] defaults MUST equal this package's defaultDebounceMS
// / defaultMaxDirs. config deliberately does not import watcher (to avoid an
// import edge), so it hard-codes the values; this test, living in the watcher
// package, is the one place that can see both and fail loudly on drift.
func TestWatcherDefaultsMatchConfigPackage(t *testing.T) {
	// Clear any ambient watcher env overrides so Load returns the pure defaults.
	t.Setenv(EnvEnabled, "")
	t.Setenv(EnvDebounceMS, "")
	t.Setenv(EnvMaxDirs, "")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Watcher.DebounceMS != defaultDebounceMS {
		t.Errorf("config Watcher.DebounceMS = %d; want defaultDebounceMS = %d", cfg.Watcher.DebounceMS, defaultDebounceMS)
	}
	if cfg.Watcher.MaxDirs != defaultMaxDirs {
		t.Errorf("config Watcher.MaxDirs = %d; want defaultMaxDirs = %d", cfg.Watcher.MaxDirs, defaultMaxDirs)
	}
	if cfg.Watcher.Enabled {
		t.Error("config Watcher.Enabled = true; want false (watcher off by default)")
	}
}
