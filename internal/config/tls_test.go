package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_TLSFromFile(t *testing.T) {
	isolateEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[server.tls]
cert_file = "/etc/ssl/cert.pem"
key_file = "/etc/ssl/key.pem"
redirect_http = ":80"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Server.TLS.CertFile != "/etc/ssl/cert.pem" {
		t.Errorf("cert_file = %q", cfg.Server.TLS.CertFile)
	}
	if cfg.Server.TLS.KeyFile != "/etc/ssl/key.pem" {
		t.Errorf("key_file = %q", cfg.Server.TLS.KeyFile)
	}
	if cfg.Server.TLS.RedirectHTTP != ":80" {
		t.Errorf("redirect_http = %q", cfg.Server.TLS.RedirectHTTP)
	}
	if !cfg.Server.TLS.Enabled() {
		t.Error("Enabled() = false; want true with cert+key set")
	}
}

func TestLoad_TLSDefaultDisabled(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Server.TLS.Enabled() {
		t.Error("TLS Enabled() = true; want false by default")
	}
	if cfg.Server.TLS.SelfSigned {
		t.Error("self_signed defaults to true; want false")
	}
}

func TestLoad_TLSSelfSignedFromFile(t *testing.T) {
	isolateEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[server.tls]\nself_signed = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.Server.TLS.SelfSigned || !cfg.Server.TLS.Enabled() {
		t.Errorf("self_signed=%v Enabled=%v; want both true", cfg.Server.TLS.SelfSigned, cfg.Server.TLS.Enabled())
	}
}

func TestLoad_TLSEnvOverride(t *testing.T) {
	isolateEnv(t)

	t.Setenv("MXLRC_TLS_CERT_FILE", "/env/cert.pem")
	t.Setenv("MXLRC_TLS_KEY_FILE", "/env/key.pem")
	t.Setenv("MXLRC_TLS_REDIRECT_HTTP", ":8080")

	cfg, applied, err := LoadWithSources(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("LoadWithSources returned error: %v", err)
	}
	if cfg.Server.TLS.CertFile != "/env/cert.pem" || cfg.Server.TLS.KeyFile != "/env/key.pem" {
		t.Errorf("env cert/key not applied: %+v", cfg.Server.TLS)
	}
	if cfg.Server.TLS.RedirectHTTP != ":8080" {
		t.Errorf("redirect_http = %q; want :8080", cfg.Server.TLS.RedirectHTTP)
	}
	for _, k := range []string{"server.tls.cert_file", "server.tls.key_file", "server.tls.redirect_http"} {
		if !applied[k] {
			t.Errorf("expected %s marked applied", k)
		}
	}
}

func TestLoad_TLSSelfSignedEnvOverride(t *testing.T) {
	isolateEnv(t)

	t.Setenv("MXLRC_TLS_SELF_SIGNED", "true")
	cfg, applied, err := LoadWithSources(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("LoadWithSources returned error: %v", err)
	}
	if !cfg.Server.TLS.SelfSigned {
		t.Error("MXLRC_TLS_SELF_SIGNED=true not applied")
	}
	if !applied["server.tls.self_signed"] {
		t.Error("expected server.tls.self_signed marked applied")
	}
}

func TestLoad_TLSSelfSignedInvalidEnvIgnored(t *testing.T) {
	isolateEnv(t)

	t.Setenv("MXLRC_TLS_SELF_SIGNED", "not-a-bool")
	cfg, applied, err := LoadWithSources(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("LoadWithSources returned error: %v", err)
	}
	if cfg.Server.TLS.SelfSigned {
		t.Error("invalid bool should leave self_signed at its default (false)")
	}
	if applied["server.tls.self_signed"] {
		t.Error("rejected env value must not be marked applied")
	}
}

func TestLoad_TLSSelfSignedHostsFromFile(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[server.tls]\nself_signed = true\nself_signed_hosts = [\"nas.local\", \"192.168.1.10\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.Server.TLS.SelfSignedHosts) != 2 {
		t.Errorf("SelfSignedHosts = %v; want [nas.local 192.168.1.10]", cfg.Server.TLS.SelfSignedHosts)
	}
}

func TestLoad_TLSSelfSignedHostsEnvOverride(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_TLS_SELF_SIGNED_HOSTS", "nas.local,192.168.1.10")
	cfg, applied, err := LoadWithSources(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("LoadWithSources returned error: %v", err)
	}
	if len(cfg.Server.TLS.SelfSignedHosts) != 2 {
		t.Errorf("SelfSignedHosts = %v; want 2 entries", cfg.Server.TLS.SelfSignedHosts)
	}
	if !applied["server.tls.self_signed_hosts"] {
		t.Error("expected server.tls.self_signed_hosts marked applied")
	}
}

func TestLoad_TLSSelfSignedHostsValidationRejectsInvalid(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[server.tls]\nself_signed = true\nself_signed_hosts = [\"not a valid host!\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error for invalid host entry, got nil")
	}
	if !strings.Contains(err.Error(), "self_signed_hosts") {
		t.Errorf("error = %q; want mention of self_signed_hosts", err.Error())
	}
}

func TestLoad_TLSSelfSignedHostsValidationAcceptsValidEntries(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[server.tls]\nself_signed = true\nself_signed_hosts = [\"nas.local\", \"myhost\", \"192.168.1.10\", \"2001:db8::1\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.Server.TLS.SelfSignedHosts) != 4 {
		t.Errorf("SelfSignedHosts = %v; want 4 entries", cfg.Server.TLS.SelfSignedHosts)
	}
}

func TestLoad_TLSValidationFailsFast(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "self_signed with cert_file",
			body: "[server.tls]\nself_signed = true\ncert_file = \"/c.pem\"\nkey_file = \"/k.pem\"\n",
			want: "mutually exclusive",
		},
		{
			name: "cert without key",
			body: "[server.tls]\ncert_file = \"/c.pem\"\n",
			want: "must be set together",
		},
		{
			name: "key without cert",
			body: "[server.tls]\nkey_file = \"/k.pem\"\n",
			want: "must be set together",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected fail-fast error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q; want it to mention %q", err.Error(), tc.want)
			}
		})
	}
}
