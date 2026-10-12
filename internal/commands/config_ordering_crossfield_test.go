package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// writeConfigTOML writes a minimal config file and returns its path.
func writeConfigTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// assertConfigSetAccepted runs one `config set` against path and fails unless
// it exits 0 and the reloaded file holds a detector-first ordering together
// with parallel dispatch.
func assertConfigSetAccepted(t *testing.T, path, key, value string) {
	t.Helper()
	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{Key: key, Value: value, ConfigPath: path}})
	if code != 0 {
		t.Fatalf("config set %s %s: exit code = %d (output %q); want 0", key, value, code, out.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload after config set %s %s: %v", key, value, err)
	}
	if cfg.InstrumentalDetector.Ordering != "front" || cfg.Providers.Mode != "parallel" {
		t.Fatalf("saved ordering/mode = %q/%q; want front/parallel",
			cfg.InstrumentalDetector.Ordering, cfg.Providers.Mode)
	}
}

// TestConfigSetAcceptsOrderingFrontWhenModeParallel verifies `config set`
// writes a detector-first ordering onto a config that already dispatches in
// parallel. Parallel dispatch stages the detector outside the provider race
// (#528), so the pair is a supported configuration.
func TestConfigSetAcceptsOrderingFrontWhenModeParallel(t *testing.T) {
	path := writeConfigTOML(t, "[providers]\nmode = \"parallel\"\n")
	assertConfigSetAccepted(t, path, "instrumental_detector.ordering", "front")
}

// TestConfigSetAcceptsModeParallelWhenOrderingFront reaches the same pair by
// setting the other key last.
func TestConfigSetAcceptsModeParallelWhenOrderingFront(t *testing.T) {
	path := writeConfigTOML(t, "[instrumental_detector]\nordering = \"front\"\n")
	assertConfigSetAccepted(t, path, "providers.mode", "parallel")
}

// TestConfigSetTitleModeAliasStoresAll: the deprecated strong_weak spelling is
// accepted by `config set` and the FILE holds the canonical all.
func TestConfigSetTitleModeAliasStoresAll(t *testing.T) {
	path := writeConfigTOML(t, "[instrumental_title]\nmode = \"strong\"\n")
	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{Key: "instrumental_title.mode", Value: "strong_weak", ConfigPath: path}})
	if code != 0 {
		t.Fatalf("config set strong_weak: exit code = %d (output %q); want 0", code, out.String())
	}
	raw, err := os.ReadFile(path) //nolint:gosec // reason: G304: test temp path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `mode = "all"`) || strings.Contains(string(raw), "strong_weak") {
		t.Errorf("file does not hold the canonical value:\n%s", raw)
	}
}
