package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretsKeyFileEnvOverride(t *testing.T) {
	t.Setenv("MXLRC_SECRETS_KEY_FILE", "/run/secrets/mxlrcgo_key")
	cfg, applied, err := LoadWithSources("")
	if err != nil {
		t.Fatalf("LoadWithSources: %v", err)
	}
	if cfg.Secrets.KeyFile != "/run/secrets/mxlrcgo_key" {
		t.Fatalf("KeyFile = %q, want /run/secrets/mxlrcgo_key", cfg.Secrets.KeyFile)
	}
	if !applied["secrets.key_file"] {
		t.Fatalf("secrets.key_file env override not recorded as applied")
	}
}

func TestSecretsKeyOptionsDefaultPath(t *testing.T) {
	t.Setenv("MXLRC_DOCKER", "")
	t.Setenv("MXLRC_SECRETS_KEY_FILE", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cfg := defaults()
	opts := cfg.SecretsKeyOptions()
	if !strings.HasSuffix(opts.KeyFilePath, filepath.Join("mxlrcgo-svc", ".mxlrcgo.key")) {
		t.Fatalf("default KeyFilePath = %q, want a path ending in mxlrcgo-svc/.mxlrcgo.key", opts.KeyFilePath)
	}
	if opts.MasterKeyB64 != "" {
		t.Fatalf("MasterKeyB64 must be left empty for the caller to fill from env")
	}
}

// TestSecretsKeyOptionsExplicitPath verifies that a custom key_file config
// value is passed through as KeyFilePath regardless of Docker env state.
func TestSecretsKeyOptionsExplicitPath(t *testing.T) {
	for _, docker := range []string{"", "true"} {
		t.Run("MXLRC_DOCKER="+docker, func(t *testing.T) {
			t.Setenv("MXLRC_DOCKER", docker)
			cfg := Config{Secrets: SecretsConfig{KeyFile: "/custom/key"}}
			opts := cfg.SecretsKeyOptions()
			if opts.KeyFilePath != "/custom/key" {
				t.Fatalf("KeyFilePath = %q, want /custom/key", opts.KeyFilePath)
			}
		})
	}
}

// TestRedactionParityForDBSourcedSecrets verifies that a token/webhook value
// (such as one sourced from the encrypted DB store and placed into the banner
// config) is redacted in the config dump exactly like a plaintext value.
func TestRedactionParityForDBSourcedSecrets(t *testing.T) {
	cfg := defaults()
	cfg.API.Token = "db-sourced-token"
	cfg.Server.WebhookAPIKeys = []string{"db-sourced-webhook"}

	text := FormatConfigText(cfg, nil, nil)
	if strings.Contains(text, "db-sourced-token") {
		t.Fatalf("DB-sourced token leaked into config dump:\n%s", text)
	}
	if strings.Contains(text, "db-sourced-webhook") {
		t.Fatalf("DB-sourced webhook key leaked into config dump:\n%s", text)
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in config dump:\n%s", text)
	}
	// The key-file path itself is informational and shown (not the key bytes).
	cfg.Secrets.KeyFile = "/data/.mxlrcgo.key"
	text = FormatConfigText(cfg, nil, nil)
	if !strings.Contains(text, "key_file = /data/.mxlrcgo.key") {
		t.Fatalf("expected secrets.key_file path in config dump:\n%s", text)
	}
}
