package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_TrustedNetworksFromFile(t *testing.T) {
	isolateEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[server.trusted_networks]
cidrs = ["192.168.1.0/24", "10.0.0.0/8"]
trusted_proxies = ["172.16.0.0/12"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.Server.TrustedNetworks.Cidrs; len(got) != 2 || got[0] != "192.168.1.0/24" || got[1] != "10.0.0.0/8" {
		t.Errorf("cidrs = %v; want [192.168.1.0/24 10.0.0.0/8]", got)
	}
	if got := cfg.Server.TrustedNetworks.TrustedProxies; len(got) != 1 || got[0] != "172.16.0.0/12" {
		t.Errorf("trusted_proxies = %v; want [172.16.0.0/12]", got)
	}
}

func TestLoad_TrustedNetworksDefaultEmpty(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.Server.TrustedNetworks.Cidrs) != 0 {
		t.Errorf("default cidrs = %v; want empty", cfg.Server.TrustedNetworks.Cidrs)
	}
	if len(cfg.Server.TrustedNetworks.TrustedProxies) != 0 {
		t.Errorf("default trusted_proxies = %v; want empty", cfg.Server.TrustedNetworks.TrustedProxies)
	}
}

func TestLoad_TrustedNetworksEnvOverride(t *testing.T) {
	isolateEnv(t)

	t.Setenv("MXLRC_TRUSTED_CIDRS", "192.168.0.0/16, 10.0.0.0/8")
	t.Setenv("MXLRC_TRUSTED_PROXIES", "172.16.0.0/12")

	cfg, applied, err := LoadWithSources(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("LoadWithSources returned error: %v", err)
	}
	if got := cfg.Server.TrustedNetworks.Cidrs; len(got) != 2 || got[0] != "192.168.0.0/16" || got[1] != "10.0.0.0/8" {
		t.Errorf("env cidrs = %v; want [192.168.0.0/16 10.0.0.0/8]", got)
	}
	if got := cfg.Server.TrustedNetworks.TrustedProxies; len(got) != 1 || got[0] != "172.16.0.0/12" {
		t.Errorf("env trusted_proxies = %v; want [172.16.0.0/12]", got)
	}
	if !applied["server.trusted_networks.cidrs"] {
		t.Error("expected server.trusted_networks.cidrs marked applied")
	}
	if !applied["server.trusted_networks.trusted_proxies"] {
		t.Error("expected server.trusted_networks.trusted_proxies marked applied")
	}
}

func TestLoad_TrustedNetworksEnvOverridesFile(t *testing.T) {
	isolateEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[server.trusted_networks]
cidrs = ["192.168.1.0/24"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MXLRC_TRUSTED_CIDRS", "10.10.0.0/16")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	// env takes precedence over file (CLI > env > file; no CLI here).
	if got := cfg.Server.TrustedNetworks.Cidrs; len(got) != 1 || got[0] != "10.10.0.0/16" {
		t.Errorf("cidrs = %v; want [10.10.0.0/16] (env overrides file)", got)
	}
}

func TestLoad_TrustedNetworksInvalidCIDRFailsFast(t *testing.T) {
	cases := []struct {
		name string
		body string
		env  map[string]string
		want string
	}{
		{
			name: "invalid cidr in file",
			body: "[server.trusted_networks]\ncidrs = [\"not-a-cidr\"]\n",
			want: "trusted_networks.cidrs",
		},
		{
			name: "invalid proxy cidr in file",
			body: "[server.trusted_networks]\ntrusted_proxies = [\"10.0.0.0/99\"]\n",
			want: "trusted_networks.trusted_proxies",
		},
		{
			name: "invalid cidr via env",
			env:  map[string]string{"MXLRC_TRUSTED_CIDRS": "garbage"},
			want: "trusted_networks.cidrs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			path := filepath.Join(t.TempDir(), "nonexistent.toml")
			if tc.body != "" {
				path = filepath.Join(t.TempDir(), "config.toml")
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected fail-fast error for invalid CIDR, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q; want it to mention %q", err.Error(), tc.want)
			}
		})
	}
}
