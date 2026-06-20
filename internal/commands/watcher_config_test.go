package commands

import (
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
)

// TestWatcherConfigFromCentral verifies the central [watcher] config maps onto
// the watcher package Config, converting DebounceMS (ms) to a time.Duration.
func TestWatcherConfigFromCentral(t *testing.T) {
	cfg := config.Config{}
	cfg.Watcher.Enabled = true
	cfg.Watcher.DebounceMS = 1500
	cfg.Watcher.MaxDirs = 7

	got := watcherConfigFromCentral(cfg)
	if got.Enabled != true {
		t.Error("Enabled = false; want true")
	}
	if got.Debounce != 1500*time.Millisecond {
		t.Errorf("Debounce = %v; want 1.5s", got.Debounce)
	}
	if got.MaxDirs != 7 {
		t.Errorf("MaxDirs = %d; want 7", got.MaxDirs)
	}
}

// TestWatcherConfigFromCentralClampDeferred verifies a non-positive DebounceMS
// or MaxDirs is passed through as-is (zero), leaving the clamp to watcher.New
// (which raises it to the package default). This preserves the documented New()
// clamp semantics: the mapping is a straight translation.
func TestWatcherConfigFromCentralClampDeferred(t *testing.T) {
	cfg := config.Config{}
	cfg.Watcher.Enabled = true
	cfg.Watcher.DebounceMS = 0
	cfg.Watcher.MaxDirs = 0

	wc := watcherConfigFromCentral(cfg)
	if wc.Enabled != true {
		t.Error("Enabled = false; want true")
	}
	if wc.Debounce != 0 {
		t.Errorf("Debounce = %v; want 0 (clamp deferred to watcher.New)", wc.Debounce)
	}
	if wc.MaxDirs != 0 {
		t.Errorf("MaxDirs = %d; want 0 (clamp deferred to watcher.New)", wc.MaxDirs)
	}
}
