package commands

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// TestConfigSetOverRetiredBudgetPerCycle pins #1324 on the `config set` tier.
// Every earlier `config set` re-encoded the whole struct, so it wrote
// word_sync_generate.budget_per_cycle into the file; that leftover (here
// wrong-typed) must not stop a later `config set`, and the re-encode drops it.
func TestConfigSetOverRetiredBudgetPerCycle(t *testing.T) {
	isolateCommandsEnv(t)
	path := writeConfigTOML(t, "[word_sync_generate]\nbudget_per_cycle = \"lots\"\n")
	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "upgrade_sweep.batch", Value: "7", ConfigPath: path,
	}})
	if code != 0 {
		t.Fatalf("config set over a leftover: exit %d; want 0 (output: %q)", code, out.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "budget_per_cycle") {
		t.Errorf("config set kept the retired key:\n%s", data)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load after config set: %v", err)
	}
	if cfg.UpgradeSweep.Batch != 7 {
		t.Errorf("upgrade_sweep.batch = %d; want 7", cfg.UpgradeSweep.Batch)
	}
}
