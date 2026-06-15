package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/secrets"
)

// stdinFile writes content to a temp file and returns it opened for reading, so
// runSecretsSet can read a "piped" value from a real *os.File (not a TTY).
func stdinFile(t *testing.T, content string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write stdin file: %v", err)
	}
	f, err := os.Open(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("open stdin file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestRunSecretsImport_BothFromConfig(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	cfg := config.Config{}
	cfg.API.Token = "effective-token"
	cfg.Server.WebhookAPIKeys = []string{"effective-webhook"}

	var out bytes.Buffer
	if code := runSecretsImport(ctx, &out, cfg, store, SecretsImportCmd{}); code != 0 {
		t.Fatalf("import exit code = %d, want 0", code)
	}

	// Both secrets written and round-trip via Get.
	if got, ok, _ := store.Get(ctx, secrets.NameMusixmatchToken); !ok || got != "effective-token" {
		t.Fatalf("token Get = (%q, %v), want (effective-token, true)", got, ok)
	}
	if got, ok, _ := store.Get(ctx, secrets.NameWebhookAPIKey); !ok || got != "effective-webhook" {
		t.Fatalf("webhook Get = (%q, %v), want (effective-webhook, true)", got, ok)
	}
	if !strings.Contains(out.String(), "Remove the now-redundant plaintext") {
		t.Fatalf("import output missing remove-plaintext reminder: %q", out.String())
	}
	// Never echoes the value.
	if strings.Contains(out.String(), "effective-token") || strings.Contains(out.String(), "effective-webhook") {
		t.Fatalf("import echoed a secret value: %q", out.String())
	}
}

func TestRunSecretsImport_WritesEncrypted(t *testing.T) {
	// A store backed by a real DB: verify the raw ciphertext column != plaintext.
	ctx := context.Background()
	store := newSecretStore(t)
	cfg := config.Config{}
	cfg.API.Token = "secret-plaintext-xyz"

	var out bytes.Buffer
	if code := runSecretsImport(ctx, &out, cfg, store, SecretsImportCmd{Token: true}); code != 0 {
		t.Fatalf("import exit code = %d, want 0", code)
	}
	if got, ok, _ := store.Get(ctx, secrets.NameMusixmatchToken); !ok || got != "secret-plaintext-xyz" {
		t.Fatalf("token Get = (%q, %v), want round-trip", got, ok)
	}
	// Webhook not imported (token-only scope).
	if _, ok, _ := store.Get(ctx, secrets.NameWebhookAPIKey); ok {
		t.Fatalf("webhook imported despite --token scope")
	}
}

func TestRunSecretsImport_TokenScopeOnly(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	cfg := config.Config{}
	cfg.API.Token = "tok"
	cfg.Server.WebhookAPIKeys = []string{"wh"}

	var out bytes.Buffer
	if code := runSecretsImport(ctx, &out, cfg, store, SecretsImportCmd{Token: true}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if _, ok, _ := store.Get(ctx, secrets.NameMusixmatchToken); !ok {
		t.Fatalf("token not imported under --token")
	}
	if _, ok, _ := store.Get(ctx, secrets.NameWebhookAPIKey); ok {
		t.Fatalf("webhook imported under --token scope")
	}
}

func TestRunSecretsImport_WebhookScopeOnly(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	cfg := config.Config{}
	cfg.API.Token = "tok"
	cfg.Server.WebhookAPIKeys = []string{"wh"}

	var out bytes.Buffer
	if code := runSecretsImport(ctx, &out, cfg, store, SecretsImportCmd{Webhook: true}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if _, ok, _ := store.Get(ctx, secrets.NameWebhookAPIKey); !ok {
		t.Fatalf("webhook not imported under --webhook")
	}
	if _, ok, _ := store.Get(ctx, secrets.NameMusixmatchToken); ok {
		t.Fatalf("token imported under --webhook scope")
	}
}

func TestRunSecretsImport_Idempotent(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	cfg := config.Config{}
	cfg.API.Token = "tok"

	var out bytes.Buffer
	if code := runSecretsImport(ctx, &out, cfg, store, SecretsImportCmd{Token: true}); code != 0 {
		t.Fatalf("first import exit = %d", code)
	}
	out.Reset()
	if code := runSecretsImport(ctx, &out, cfg, store, SecretsImportCmd{Token: true}); code != 0 {
		t.Fatalf("second import exit = %d", code)
	}
	infos, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("re-import created %d rows, want 1 (idempotent upsert)", len(infos))
	}
}

func TestRunSecretsImport_SkipsAbsent(t *testing.T) {
	// No effective value for either secret: both skipped, no rows written, no
	// remove-plaintext reminder (nothing imported).
	ctx := context.Background()
	store := newSecretStore(t)

	var out bytes.Buffer
	if code := runSecretsImport(ctx, &out, config.Config{}, store, SecretsImportCmd{}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	infos, _ := store.List(ctx)
	if len(infos) != 0 {
		t.Fatalf("absent import wrote %d rows, want 0", len(infos))
	}
	if !strings.Contains(out.String(), "skipping "+secrets.NameMusixmatchToken) {
		t.Fatalf("missing skip message for token: %q", out.String())
	}
	if strings.Contains(out.String(), "Remove the now-redundant plaintext") {
		t.Fatalf("printed remove reminder despite importing nothing")
	}
}

func TestRunSecretsImport_DoesNotReadDBAsSource(t *testing.T) {
	// The DB already holds a token, but cfg (higher tiers) is empty. import must
	// NOT read the DB and re-write it; it should skip with "no effective value".
	ctx := context.Background()
	store := newSecretStore(t)
	if err := store.Set(ctx, secrets.NameMusixmatchToken, "db-only-token"); err != nil {
		t.Fatalf("seed Set: %v", err)
	}

	var out bytes.Buffer
	if code := runSecretsImport(ctx, &out, config.Config{}, store, SecretsImportCmd{Token: true}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "skipping "+secrets.NameMusixmatchToken) {
		t.Fatalf("import read the DB as a source instead of skipping: %q", out.String())
	}
	// Existing DB value is left intact (not overwritten with empty).
	if got, ok, _ := store.Get(ctx, secrets.NameMusixmatchToken); !ok || got != "db-only-token" {
		t.Fatalf("DB value changed: (%q, %v)", got, ok)
	}
}

func TestRunSecretsSet_RejectsArgvValue(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	var out bytes.Buffer
	args := SecretsSetCmd{Name: secrets.NameMusixmatchToken, Value: "leaked-on-argv"}
	if code := runSecretsSet(ctx, &out, store, args, stdinFile(t, "")); code != 2 {
		t.Fatalf("argv value: exit code = %d, want 2", code)
	}
	if _, ok, _ := store.Get(ctx, secrets.NameMusixmatchToken); ok {
		t.Fatalf("argv-passed value was stored; must be rejected")
	}
}

func TestRunSecretsSet_FromStdin(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	var out bytes.Buffer
	args := SecretsSetCmd{Name: secrets.NameWebhookAPIKey}
	if code := runSecretsSet(ctx, &out, store, args, stdinFile(t, "piped-secret\n")); code != 0 {
		t.Fatalf("stdin set: exit code = %d, want 0", code)
	}
	if got, ok, _ := store.Get(ctx, secrets.NameWebhookAPIKey); !ok || got != "piped-secret" {
		t.Fatalf("Get = (%q, %v), want (piped-secret, true)", got, ok)
	}
	if strings.Contains(out.String(), "piped-secret") {
		t.Fatalf("set echoed the value: %q", out.String())
	}
}

func TestRunSecretsSet_MultilineStdin(t *testing.T) {
	// A piped value that contains an interior newline must be stored in full,
	// not truncated at the first '\n'. The trailing newline is trimmed (same as
	// a single-line piped value) but the interior newline is preserved.
	ctx := context.Background()
	store := newSecretStore(t)
	var out bytes.Buffer
	args := SecretsSetCmd{Name: secrets.NameWebhookAPIKey}
	if code := runSecretsSet(ctx, &out, store, args, stdinFile(t, "line1\nline2\n")); code != 0 {
		t.Fatalf("multiline stdin set: exit code = %d, want 0", code)
	}
	got, ok, _ := store.Get(ctx, secrets.NameWebhookAPIKey)
	if !ok {
		t.Fatal("Get: secret not found after set")
	}
	const want = "line1\nline2"
	if got != want {
		t.Fatalf("Get = %q, want %q (piped value must not be truncated at first newline)", got, want)
	}
}

func TestRunSecretsSet_UnknownName(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	var out bytes.Buffer
	args := SecretsSetCmd{Name: "not_a_secret"}
	if code := runSecretsSet(ctx, &out, store, args, stdinFile(t, "x\n")); code != 2 {
		t.Fatalf("unknown name: exit code = %d, want 2", code)
	}
}

func TestRunSecretsSet_EmptyStdin(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	var out bytes.Buffer
	args := SecretsSetCmd{Name: secrets.NameMusixmatchToken}
	if code := runSecretsSet(ctx, &out, store, args, stdinFile(t, "")); code != 2 {
		t.Fatalf("empty stdin: exit code = %d, want 2", code)
	}
}

func TestRunSecretsList_Empty(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	var out bytes.Buffer
	if code := runSecretsList(ctx, &out, store); code != 0 {
		t.Fatalf("list exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "no secrets stored") {
		t.Fatalf("empty list output = %q, want friendly message", out.String())
	}
}

func TestRunSecretsList_NamesAndUpdatedAtNeverValues(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	if err := store.Set(ctx, secrets.NameMusixmatchToken, "value-must-not-appear"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var out bytes.Buffer
	if code := runSecretsList(ctx, &out, store); code != 0 {
		t.Fatalf("list exit = %d, want 0", code)
	}
	s := out.String()
	if !strings.Contains(s, secrets.NameMusixmatchToken) {
		t.Fatalf("list missing name: %q", s)
	}
	if strings.Contains(s, "value-must-not-appear") {
		t.Fatalf("list LEAKED the secret value: %q", s)
	}
}

// withMasterKeyConfig sets MXLRC_MASTER_KEY and writes a serve config pointing at
// a temp DB, returning the config path. Native mode (MXLRC_DOCKER unset).
func withMasterKeyConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("MXLRC_DOCKER", "")
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	t.Setenv("MXLRC_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeServeConfig(t, cfgPath, filepath.Join(dir, "secrets.db"), false, "")
	return cfgPath
}

// TestRunSecrets_DispatchImportThroughConfig drives runSecrets through the import
// branch with a token set in the config TOML.
func TestRunSecrets_DispatchImportThroughConfig(t *testing.T) {
	cfgPath := withMasterKeyConfig(t)
	// Append a token to the config so import has an effective value.
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}
	if _, err := f.WriteString("\n[api]\ntoken = \"cfg-token\"\n"); err != nil {
		t.Fatalf("append token: %v", err)
	}
	_ = f.Close()

	var out bytes.Buffer
	code := runSecrets(context.Background(), &out, SecretsCmd{Import: &SecretsImportCmd{Token: true, ConfigPath: cfgPath}})
	if code != 0 {
		t.Fatalf("runSecrets import exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "imported "+secrets.NameMusixmatchToken) {
		t.Fatalf("import output = %q", out.String())
	}
}

// TestRunSecrets_DispatchSetThroughConfig drives runSecrets through the set branch
// with the value piped via stdin (the process stdin is redirected to a temp file).
func TestRunSecrets_DispatchSetThroughConfig(t *testing.T) {
	cfgPath := withMasterKeyConfig(t)

	// Redirect the process stdin to a piped value for the duration of this test.
	orig := os.Stdin
	os.Stdin = stdinFile(t, "stdin-token\n")
	t.Cleanup(func() { os.Stdin = orig })

	var out bytes.Buffer
	code := runSecrets(context.Background(), &out,
		SecretsCmd{Set: &SecretsSetCmd{Name: secrets.NameMusixmatchToken, ConfigPath: cfgPath}})
	if code != 0 {
		t.Fatalf("runSecrets set exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "set "+secrets.NameMusixmatchToken) {
		t.Fatalf("set output = %q", out.String())
	}
}

// TestRunSecrets_DispatchListThroughRun drives the full top-level Run dispatch
// ("secrets list ...") through config load, DB open, store build via
// MXLRC_MASTER_KEY, and the list branch on an empty store, covering the dispatch
// case and the known-subcommand allowlist entry end-to-end.
func TestRunSecrets_DispatchListThroughRun(t *testing.T) {
	cfgPath := withMasterKeyConfig(t)
	var out bytes.Buffer
	code := Run(context.Background(), []string{"secrets", "list", "--config", cfgPath}, &out, Deps{})
	if code != 0 {
		t.Fatalf("Run secrets list exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "no secrets stored") {
		t.Fatalf("output = %q, want empty-store message", out.String())
	}
}

func TestRunSecrets_ConfigLoadError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(cfgPath, []byte("this is = not valid = toml ]["), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	var out bytes.Buffer
	code := runSecrets(context.Background(), &out, SecretsCmd{List: &SecretsListCmd{ConfigPath: cfgPath}})
	if code != 1 {
		t.Fatalf("config load error exit = %d, want 1", code)
	}
}

func TestRunSecrets_StoreInitError(t *testing.T) {
	t.Setenv("MXLRC_DOCKER", "")
	t.Setenv("MXLRC_MASTER_KEY", "not-valid-base64!!!")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeServeConfig(t, cfgPath, filepath.Join(dir, "secrets.db"), false, "")
	var out bytes.Buffer
	code := runSecrets(context.Background(), &out, SecretsCmd{List: &SecretsListCmd{ConfigPath: cfgPath}})
	if code != 1 {
		t.Fatalf("store init error exit = %d, want 1", code)
	}
}

func TestRunSecretsList_StoreError(t *testing.T) {
	var out bytes.Buffer
	if code := runSecretsList(context.Background(), &out, closedStore(t)); code != 1 {
		t.Fatalf("list store error exit = %d, want 1", code)
	}
}

func TestRunSecretsImport_StoreError(t *testing.T) {
	cfg := config.Config{}
	cfg.API.Token = "tok"
	var out bytes.Buffer
	if code := runSecretsImport(context.Background(), &out, cfg, closedStore(t), SecretsImportCmd{Token: true}); code != 1 {
		t.Fatalf("import store error exit = %d, want 1", code)
	}
}

func TestRunSecretsImport_WebhookStoreError(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.WebhookAPIKeys = []string{"wh"}
	var out bytes.Buffer
	if code := runSecretsImport(context.Background(), &out, cfg, closedStore(t), SecretsImportCmd{Webhook: true}); code != 1 {
		t.Fatalf("webhook import store error exit = %d, want 1", code)
	}
}

func TestRunSecretsSet_StoreError(t *testing.T) {
	var out bytes.Buffer
	args := SecretsSetCmd{Name: secrets.NameMusixmatchToken}
	if code := runSecretsSet(context.Background(), &out, closedStore(t), args, stdinFile(t, "val\n")); code != 1 {
		t.Fatalf("set store error exit = %d, want 1", code)
	}
}

// TestRunSecrets_BareSecrets exercises the top-level "secrets" command with no
// nested subcommand. go-arg returns a usage error (exit 2); the missing-
// subcommand default branch is also exit 2.
func TestRunSecrets_BareSecrets(t *testing.T) {
	var out bytes.Buffer
	code := Run(context.Background(), []string{"secrets"}, &out, Deps{})
	if code != 2 {
		t.Fatalf("bare secrets exit = %d, want 2", code)
	}
}
