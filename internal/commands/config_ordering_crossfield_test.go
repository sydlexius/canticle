package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// TestConfigSetRejectsOrderingFrontWhenModeParallel verifies that `config set`
// revalidates the MERGED config before persisting. Setting ordering=front on a
// config that already has mode=parallel produces the contradictory combination
// that load-time validation rejects; without a re-check here the bad value is
// written to disk and the operator only discovers it as a confusing boot
// failure on the NEXT startup.
func TestConfigSetRejectsOrderingFrontWhenModeParallel(t *testing.T) {
	path := writeConfigTOML(t, "[providers]\nmode = \"parallel\"\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "instrumental_detector.ordering", Value: "front", ConfigPath: path,
	}})

	if code != 2 {
		t.Fatalf("exit code = %d; want 2 (the contradictory combination must be rejected)", code)
	}
	if !strings.Contains(out.String(), "requires providers.mode=ordered") {
		t.Fatalf("output = %q; want the cross-field error naming both keys", out.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read config: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("config file was rewritten despite the rejection:\nbefore=%q\nafter=%q", before, after)
	}
}

// TestConfigSetRejectsModeParallelWhenOrderingFront covers the SYMMETRIC case:
// the same contradiction is reachable by changing the other field. This is the
// half a per-key validator most easily misses, since neither key is invalid on
// its own.
func TestConfigSetRejectsModeParallelWhenOrderingFront(t *testing.T) {
	path := writeConfigTOML(t, "[instrumental_detector]\nordering = \"front\"\n")

	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "providers.mode", Value: "parallel", ConfigPath: path,
	}})

	if code != 2 {
		t.Fatalf("exit code = %d; want 2 (setting mode=parallel under ordering=front must be rejected)", code)
	}
}

// TestConfigSetAllowsOrderingFrontWhenModeOrdered is the positive control: the
// cross-field check must reject only the contradictory pair, not every write to
// these keys.
func TestConfigSetAllowsOrderingFrontWhenModeOrdered(t *testing.T) {
	path := writeConfigTOML(t, "[providers]\nmode = \"ordered\"\n")

	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "instrumental_detector.ordering", Value: "front", ConfigPath: path,
	}})

	if code != 0 {
		t.Fatalf("exit code = %d (output %q); want 0 - front+ordered is a valid combination", code, out.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(saved), "front") {
		t.Fatalf("saved config = %q; want the accepted ordering=front persisted", saved)
	}
}
