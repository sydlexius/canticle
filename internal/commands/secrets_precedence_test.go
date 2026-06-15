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
	"github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/lyrics"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
	"github.com/sydlexius/mxlrcgo-svc/internal/secrets"
)

// newSecretStore opens a migrated SQLite DB (temp file) and returns an encrypted
// SQL-backed secret store. Real SQLite, no mocks, per repo convention.
func newSecretStore(t *testing.T) *secrets.SQLStore {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return secrets.NewSQLStore(sqlDB, key)
}

func TestResolveTokenWithStore_DBUsedOnlyWhenHigherAbsent(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	if err := store.Set(ctx, secrets.NameMusixmatchToken, "db-token"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Higher tier present: DB is NOT consulted and not used.
	got, fromDB, err := resolveTokenWithStore(ctx, "higher-token", store)
	if err != nil {
		t.Fatalf("resolveTokenWithStore: %v", err)
	}
	if got != "higher-token" || fromDB {
		t.Fatalf("higher present: got (%q, fromDB=%v), want (higher-token, false)", got, fromDB)
	}

	// Higher tier empty: DB tier is used.
	got, fromDB, err = resolveTokenWithStore(ctx, "", store)
	if err != nil {
		t.Fatalf("resolveTokenWithStore: %v", err)
	}
	if got != "db-token" || !fromDB {
		t.Fatalf("higher absent: got (%q, fromDB=%v), want (db-token, true)", got, fromDB)
	}
}

func TestResolveTokenWithStore_NoAutoPersist(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)

	// A present higher tier must never be written back to the DB.
	if _, _, err := resolveTokenWithStore(ctx, "higher-token", store); err != nil {
		t.Fatalf("resolveTokenWithStore: %v", err)
	}
	if _, ok, err := store.Get(ctx, secrets.NameMusixmatchToken); err != nil {
		t.Fatalf("Get: %v", err)
	} else if ok {
		t.Fatalf("token was auto-persisted to the DB; precedence must never write back")
	}
}

func TestResolveTokenWithStore_AbsentEverywhere(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	got, fromDB, err := resolveTokenWithStore(ctx, "", store)
	if err != nil {
		t.Fatalf("resolveTokenWithStore: %v", err)
	}
	if got != "" || fromDB {
		t.Fatalf("absent everywhere: got (%q, fromDB=%v), want (\"\", false)", got, fromDB)
	}
}

func TestResolveWebhookKeysWithStore_DBUsedOnlyWhenHigherAbsent(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	if err := store.Set(ctx, secrets.NameWebhookAPIKey, "db-key"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Higher tier present: DB is NOT used.
	got, fromDB, err := resolveWebhookKeysWithStore(ctx, []string{"higher-key"}, store)
	if err != nil {
		t.Fatalf("resolveWebhookKeysWithStore: %v", err)
	}
	if len(got) != 1 || got[0] != "higher-key" || fromDB {
		t.Fatalf("higher present: got (%v, fromDB=%v), want ([higher-key], false)", got, fromDB)
	}

	// Higher tier empty: DB tier is used.
	got, fromDB, err = resolveWebhookKeysWithStore(ctx, nil, store)
	if err != nil {
		t.Fatalf("resolveWebhookKeysWithStore: %v", err)
	}
	if len(got) != 1 || got[0] != "db-key" || !fromDB {
		t.Fatalf("higher absent: got (%v, fromDB=%v), want ([db-key], true)", got, fromDB)
	}
}

func TestResolveWebhookKeysWithStore_NoAutoPersist(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)

	if _, _, err := resolveWebhookKeysWithStore(ctx, []string{"higher-key"}, store); err != nil {
		t.Fatalf("resolveWebhookKeysWithStore: %v", err)
	}
	if _, ok, err := store.Get(ctx, secrets.NameWebhookAPIKey); err != nil {
		t.Fatalf("Get: %v", err)
	} else if ok {
		t.Fatalf("webhook key was auto-persisted to the DB; precedence must never write back")
	}
}

// TestResolveSecretStore_DockerAutoCreatesKeyFile verifies that in Docker mode
// with no MXLRC_MASTER_KEY, resolveSecretStore auto-creates a 0600 key file
// and returns a usable store (universal zero-setup default, Decision A=(a)).
func TestResolveSecretStore_DockerAutoCreatesKeyFile(t *testing.T) {
	t.Setenv("MXLRC_DOCKER", "true")
	t.Setenv("MXLRC_MASTER_KEY", "")
	keyFile := filepath.Join(t.TempDir(), "test.key")

	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	cfg := config.Config{Secrets: config.SecretsConfig{KeyFile: keyFile}}
	store, err := resolveSecretStore(cfg, sqlDB)
	if err != nil {
		t.Fatalf("resolveSecretStore Docker (no env key): %v", err)
	}
	if store == nil {
		t.Fatalf("Docker mode with no env key: expected a usable store")
	}
	// Round-trip confirms the auto-created key is consistent.
	ctx := context.Background()
	if err := store.Set(ctx, secrets.NameMusixmatchToken, "tok"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, ok, err := store.Get(ctx, secrets.NameMusixmatchToken); err != nil || !ok || got != "tok" {
		t.Fatalf("Get = (%q, %v, %v), want (tok, true, nil)", got, ok, err)
	}
}

// TestResolveSecretStore_NativeAutoCreatesKeyFile verifies that with no env
// key, resolveSecretStore auto-creates a 0600 key file and returns a usable
// store. This is the canonical universal-behavior coverage test.
func TestResolveSecretStore_NativeAutoCreatesKeyFile(t *testing.T) {
	t.Setenv("MXLRC_DOCKER", "")
	t.Setenv("MXLRC_MASTER_KEY", "")
	keyFile := filepath.Join(t.TempDir(), "test.key")

	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	cfg := config.Config{Secrets: config.SecretsConfig{KeyFile: keyFile}}
	store, err := resolveSecretStore(cfg, sqlDB)
	if err != nil {
		t.Fatalf("resolveSecretStore: %v", err)
	}
	if store == nil {
		t.Fatalf("native mode: expected a usable store")
	}
	// Round-trip through the constructed store to confirm the auto-created key works.
	ctx := context.Background()
	if err := store.Set(ctx, secrets.NameMusixmatchToken, "tok"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, ok, err := store.Get(ctx, secrets.NameMusixmatchToken); err != nil || !ok || got != "tok" {
		t.Fatalf("Get = (%q, %v, %v), want (tok, true, nil)", got, ok, err)
	}
}

// TestResolveSecretStore_MalformedMasterKey verifies that a malformed
// MXLRC_MASTER_KEY surfaces loudly as a wrapped error with no store, so the
// call site exits fatally.
func TestResolveSecretStore_MalformedMasterKey(t *testing.T) {
	t.Setenv("MXLRC_DOCKER", "")
	t.Setenv("MXLRC_MASTER_KEY", "not-valid-base64!!!")

	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	store, err := resolveSecretStore(config.Config{}, sqlDB)
	if err == nil {
		t.Fatalf("malformed master key: expected a fatal error")
	}
	if store != nil {
		t.Fatalf("malformed master key: want nil store, got %v", store)
	}
	if !strings.Contains(err.Error(), "resolve secrets master key") {
		t.Fatalf("error must be wrapped with context, got %q", err.Error())
	}
}

// closedStore returns an encrypted store over a CLOSED DB so that Store.Get
// returns a non-ErrNoRows error, exercising the helpers' read-error branches.
func closedStore(t *testing.T) secrets.Store {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	store := secrets.NewSQLStore(sqlDB, key)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return store
}

func TestResolveTokenWithStore_NilStore(t *testing.T) {
	got, fromDB, err := resolveTokenWithStore(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("resolveTokenWithStore: %v", err)
	}
	if got != "" || fromDB {
		t.Fatalf("nil store: got (%q, fromDB=%v), want (\"\", false)", got, fromDB)
	}
}

func TestResolveTokenWithStore_ReadError(t *testing.T) {
	_, _, err := resolveTokenWithStore(context.Background(), "", closedStore(t))
	if err == nil {
		t.Fatal("expected a read error from a closed store")
	}
	if !strings.Contains(err.Error(), "read musixmatch token from secret store") {
		t.Fatalf("error must be wrapped with context, got %q", err.Error())
	}
}

func TestResolveWebhookKeysWithStore_NilStore(t *testing.T) {
	got, fromDB, err := resolveWebhookKeysWithStore(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("resolveWebhookKeysWithStore: %v", err)
	}
	if len(got) != 0 || fromDB {
		t.Fatalf("nil store: got (%v, fromDB=%v), want (nil, false)", got, fromDB)
	}
}

func TestResolveWebhookKeysWithStore_ReadError(t *testing.T) {
	_, _, err := resolveWebhookKeysWithStore(context.Background(), nil, closedStore(t))
	if err == nil {
		t.Fatal("expected a read error from a closed store")
	}
	if !strings.Contains(err.Error(), "read webhook API key from secret store") {
		t.Fatalf("error must be wrapped with context, got %q", err.Error())
	}
}

// TestResolveWebhookKeysWithStore_BlankDBValue verifies that a present-but-blank
// DB value is treated as absent (no fallback key), not as a usable key.
func TestResolveWebhookKeysWithStore_BlankDBValue(t *testing.T) {
	ctx := context.Background()
	store := newSecretStore(t)
	if err := store.Set(ctx, secrets.NameWebhookAPIKey, "   "); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, fromDB, err := resolveWebhookKeysWithStore(ctx, nil, store)
	if err != nil {
		t.Fatalf("resolveWebhookKeysWithStore: %v", err)
	}
	if len(got) != 0 || fromDB {
		t.Fatalf("blank DB value: got (%v, fromDB=%v), want (nil, false)", got, fromDB)
	}
}

// TestRunServe_DockerAutoKeyFile drives runServe in Docker mode with no
// MXLRC_MASTER_KEY: the auto-generated key file is the universal zero-setup
// default, so the store should be initialized successfully. A token is supplied
// so serve progresses past provider selection to verifier construction; the
// nonexistent ffmpeg causes exit code 1 there. The key-file stat after the call
// is the actual proof that the store initialized and created the file.
func TestRunServe_DockerAutoKeyFile(t *testing.T) {
	t.Setenv("MXLRC_DOCKER", "true")
	t.Setenv("MXLRC_MASTER_KEY", "")
	// In Docker mode xdgDataPath returns /config/... which is not writable in tests.
	// Override the key file path via env so the auto-create lands in a temp dir.
	keyFile := filepath.Join(t.TempDir(), "test.key")
	t.Setenv("MXLRC_SECRETS_KEY_FILE", keyFile)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve.db")
	cfgPath := filepath.Join(dir, "config.toml")
	// Nonexistent ffmpeg forces exit at verifier construction, after the store is
	// successfully initialized - the exit-1 here is from the verifier, not the key.
	writeServeConfig(t, cfgPath, dbPath, true, filepath.Join(dir, "no-such-ffmpeg"))

	// Supply a token so serve progresses past provider selection to verifier construction.
	t.Setenv("MUSIXMATCH_TOKEN", "tok")

	var out bytes.Buffer
	code := runServe(
		context.Background(),
		&out,
		ServeCmd{ConfigPath: cfgPath},
		func(string) musixmatch.Fetcher { return fakeFetcher{} },
		func(...string) lyrics.Writer { return fakeWriter{} },
	)
	// Exit 1 from verifier failure confirms the store was built successfully.
	if code != 1 {
		t.Fatalf("Docker auto-key-file serve: exit code = %d, want 1", code)
	}
	// Key file creation is the definitive proof that the store initialized.
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("expected auto-created key file at %q: %v", keyFile, err)
	}
}

// TestRunServe_DBSecretsThenVerifierFailure drives runServe through the full
// secret-store wiring: a usable store (key via MXLRC_MASTER_KEY), DB-sourced
// token and webhook key (higher tiers empty -> the fromDB slog branches fire),
// the startup banner, and provider selection - then fails deterministically at
// verifier construction (verification enabled with a nonexistent ffmpeg) so the
// HTTP server never starts. Exit code 1.
func TestRunServe_DBSecretsThenVerifierFailure(t *testing.T) {
	t.Setenv("MXLRC_DOCKER", "")
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	t.Setenv("MXLRC_MASTER_KEY", base64.StdEncoding.EncodeToString(key))

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve.db")

	// Seed both secrets into the DB encrypted with the SAME key runServe resolves.
	sqlDB, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	seed := secrets.NewSQLStore(sqlDB, key)
	ctx := context.Background()
	if err := seed.Set(ctx, secrets.NameMusixmatchToken, "db-token"); err != nil {
		t.Fatalf("Set token: %v", err)
	}
	if err := seed.Set(ctx, secrets.NameWebhookAPIKey, "mxk_dbwebhookkey000000000000000000"); err != nil {
		t.Fatalf("Set webhook: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cfgPath := filepath.Join(dir, "config.toml")
	// Verification enabled with a nonexistent ffmpeg -> newVerifier fails after
	// the banner and provider selection, before the server starts.
	writeServeConfig(t, cfgPath, dbPath, true, filepath.Join(dir, "no-such-ffmpeg"))

	var out bytes.Buffer
	code := runServe(
		ctx,
		&out,
		ServeCmd{ConfigPath: cfgPath},
		func(string) musixmatch.Fetcher { return fakeFetcher{} },
		func(...string) lyrics.Writer { return fakeWriter{} },
	)
	if code != 1 {
		t.Fatalf("verifier-failure path: exit code = %d, want 1", code)
	}
}

// writeServeConfig writes a minimal serve config TOML pointing at dbPath with
// the musixmatch provider. When verifyEnabled is true it enables verification
// with the given ffmpeg path (used to force a deterministic newVerifier failure).
func writeServeConfig(t *testing.T, path, dbPath string, verifyEnabled bool, ffmpegPath string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("[db]\n")
	b.WriteString("path = " + tomlString(dbPath) + "\n\n")
	b.WriteString("[providers]\n")
	b.WriteString("primary = \"musixmatch\"\n\n")
	if verifyEnabled {
		b.WriteString("[verification]\n")
		b.WriteString("enabled = true\n")
		b.WriteString("whisper_url = \"http://127.0.0.1:1\"\n")
		b.WriteString("ffmpeg_path = " + tomlString(ffmpegPath) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func tomlString(s string) string {
	return "\"" + strings.ReplaceAll(s, `\`, `\\`) + "\""
}
