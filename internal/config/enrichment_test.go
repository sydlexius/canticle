package config

import "testing"

// TestLoad_EnrichmentDefault verifies recording enrichment is on by default,
// preserving the pre-#217 always-on behavior when no TOML or env override is set.
func TestLoad_EnrichmentDefault(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Enrichment.Enabled {
		t.Error("Enrichment.Enabled = false; want true (enabled by default)")
	}
}

// TestLoad_EnrichmentEnvDisabled verifies MXLRC_ENRICHMENT_ENABLED overrides the
// default off.
func TestLoad_EnrichmentEnvDisabled(t *testing.T) {
	t.Setenv("MXLRC_ENRICHMENT_ENABLED", "false")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Enrichment.Enabled {
		t.Error("Enrichment.Enabled = true; want false (env override)")
	}
}

// TestLoad_EnrichmentEnvInvalidIgnored verifies an unparsable env value leaves
// the current (default true) value untouched.
func TestLoad_EnrichmentEnvInvalidIgnored(t *testing.T) {
	t.Setenv("MXLRC_ENRICHMENT_ENABLED", "notabool")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Enrichment.Enabled {
		t.Error("Enrichment.Enabled = false; want true (invalid env ignored)")
	}
}
