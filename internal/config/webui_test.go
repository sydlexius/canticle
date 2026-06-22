package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_WebUIEnabledEnvOverride verifies MXLRC_WEB_UI_ENABLED overrides the
// file value in both directions (env > file precedence).
func TestLoad_WebUIEnabledEnvOverride(t *testing.T) {
	tests := []struct {
		name     string
		fileBody string
		env      string
		want     bool
	}{
		{"env_true_over_file_false", "[server]\nweb_ui_enabled = false\n", "true", true},
		{"env_false_over_file_true", "[server]\nweb_ui_enabled = true\n", "false", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.fileBody), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			t.Setenv("MXLRC_WEB_UI_ENABLED", tc.env)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Server.WebUIEnabled != tc.want {
				t.Errorf("Server.WebUIEnabled = %v; want %v (env override)", cfg.Server.WebUIEnabled, tc.want)
			}
		})
	}
}

// TestLoad_WebUIEnabledInvalidEnvIgnored verifies a non-bool env value leaves
// the file/default value in place.
func TestLoad_WebUIEnabledInvalidEnvIgnored(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[server]\nweb_ui_enabled = true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("MXLRC_WEB_UI_ENABLED", "maybe")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Server.WebUIEnabled {
		t.Error("Server.WebUIEnabled = false; want true (invalid env ignored, file value kept)")
	}
}

// TestLoad_WebUIEnabledDefaultsFalse verifies the field defaults to false when
// neither file nor env set it.
func TestLoad_WebUIEnabledDefaultsFalse(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.WebUIEnabled {
		t.Error("Server.WebUIEnabled = true; want false (off by default)")
	}
}

// TestWebUIEnabledRegistryEntry verifies the server.web_ui_enabled field is
// registered with the expected type, env var, tier, and editability so the
// settings UI and the env-override drift test both see it.
func TestWebUIEnabledRegistryEntry(t *testing.T) {
	f, ok := FieldByPath("server.web_ui_enabled")
	if !ok {
		t.Fatal("registry missing server.web_ui_enabled")
	}
	if f.Section != "server" {
		t.Errorf("Section = %q; want server", f.Section)
	}
	if f.Type != TypeBool {
		t.Errorf("Type = %v; want TypeBool", f.Type)
	}
	if len(f.EnvVars) != 1 || f.EnvVars[0] != "MXLRC_WEB_UI_ENABLED" {
		t.Errorf("EnvVars = %v; want [MXLRC_WEB_UI_ENABLED]", f.EnvVars)
	}
	if f.Criticality != Caution {
		t.Errorf("Criticality = %v; want Caution", f.Criticality)
	}
	if !f.Editable {
		t.Error("Editable = false; want true")
	}
	if f.Sensitive {
		t.Error("Sensitive = true; want false")
	}
}
