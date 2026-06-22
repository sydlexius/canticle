package web

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/secrets"
)

// fakeSecretStore is an in-memory secrets.Store for the write-path tests: it
// records Set calls so a test can assert a secret was routed to the store rather
// than the TOML.
type fakeSecretStore struct {
	mu   sync.Mutex
	vals map[string]string
}

func newFakeSecretStore() *fakeSecretStore { return &fakeSecretStore{vals: map[string]string{}} }

func (f *fakeSecretStore) Set(_ context.Context, name, plaintext string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vals[name] = plaintext
	return nil
}

func (f *fakeSecretStore) Get(_ context.Context, name string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vals[name]
	return v, ok, nil
}

func (f *fakeSecretStore) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vals, name)
	return nil
}

func (f *fakeSecretStore) List(_ context.Context) ([]secrets.SecretInfo, error) {
	return nil, nil
}

const seedConfigTOML = `[api]
cooldown = 5
miss_backoff_base_hours = 168

[server]
scan_interval_seconds = 900

[providers]
disabled = []

[logging]
level = "info"
max_files = 3
`

// writableTestUI builds a UI with the write path wired to a fresh temp config
// file and the given secret store, and returns the mounted handler and the file
// path. The UI is built without auth, so the routes are public (no session
// gate) and the same-origin check passes for a header-less test request.
func writableTestUI(t *testing.T, store *fakeSecretStore) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(seedConfigTOML), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(store))
	mux := http.NewServeMux()
	u.Register(mux)
	return mux, cfgPath
}

// postField issues a save POST with a valid double-submit CSRF token (cookie +
// matching form field). Extra form values are merged in.
func postField(t *testing.T, h http.Handler, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form.Set("csrf_token", testCSRFToken)
	req := httptest.NewRequest(http.MethodPost, "/settings/field", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: testCSRFToken})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSaveFieldHappyPath(t *testing.T) {
	h, cfgPath := writableTestUI(t, newFakeSecretStore())
	rec := postField(t, h, url.Values{"path": {"logging.level"}, "value": {"debug"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("logging.level = %q, want debug", cfg.Logging.Level)
	}
}

func TestSaveFieldMalformedFormRejected(t *testing.T) {
	store := newFakeSecretStore()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(seedConfigTOML), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(store))

	// The explicit r.ParseForm() guard mirrors the section save handler: a truly
	// malformed body yields a clean 400 instead of silently empty values. To
	// exercise the guard, pre-populate r.PostForm with a valid CSRF token (so
	// enforceCSRFToken's PostFormValue does not parse, and therefore swallow, the
	// request first) and put invalid percent-encoding in the query so ParseForm
	// returns a non-nil error.
	req := httptest.NewRequest(http.MethodPost, "/settings/field?bad=%zz", nil)
	req.PostForm = url.Values{"csrf_token": {testCSRFToken}}
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: testCSRFToken})
	rec := httptest.NewRecorder()
	u.handleSaveField(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "invalid form data" {
		t.Errorf("body = %q, want %q", got, "invalid form data")
	}
}

func TestSaveFieldCSRFRejected(t *testing.T) {
	h, cfgPath := writableTestUI(t, newFakeSecretStore())

	// Missing token: no cookie, no form field.
	req := httptest.NewRequest(http.MethodPost, "/settings/field",
		strings.NewReader(url.Values{"path": {"logging.level"}, "value": {"debug"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want 403", rec.Code)
	}

	// Mismatched token: cookie and form field differ.
	form := url.Values{"path": {"logging.level"}, "value": {"debug"}, "csrf_token": {testCSRFToken}}
	req2 := httptest.NewRequest(http.MethodPost, "/settings/field", strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: strings.Repeat("b", 64)})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("bad CSRF status = %d, want 403", rec2.Code)
	}

	// Neither attempt mutated the file.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("logging.level = %q, want info (no write on CSRF failure)", cfg.Logging.Level)
	}
}

func TestSaveFieldInvalidValueNoMutation(t *testing.T) {
	h, cfgPath := writableTestUI(t, newFakeSecretStore())
	rec := postField(t, h, url.Values{"path": {"logging.level"}, "value": {"bogus"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("logging.level = %q, want info (rejected value must not write)", cfg.Logging.Level)
	}
}

func TestSaveFieldSecretRoutedToStoreNotTOML(t *testing.T) {
	store := newFakeSecretStore()
	h, cfgPath := writableTestUI(t, store)

	rec := postField(t, h, url.Values{"path": {"api.token"}, "value": {"tok_SECRET_VALUE"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, _, _ := store.Get(context.Background(), "musixmatch_token"); got != "tok_SECRET_VALUE" {
		t.Errorf("token not routed to store: got %q", got)
	}
	raw, err := os.ReadFile(cfgPath) //nolint:gosec // G304: test-controlled temp path
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), "tok_SECRET_VALUE") {
		t.Error("token leaked into the config TOML; must stay in the secret store")
	}
}

func TestSaveFieldBlankSecretKeepsExisting(t *testing.T) {
	store := newFakeSecretStore()
	h, _ := writableTestUI(t, store)
	rec := postField(t, h, url.Values{"path": {"api.token"}, "value": {""}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (blank = keep)", rec.Code)
	}
	if _, ok, _ := store.Get(context.Background(), "musixmatch_token"); ok {
		t.Error("blank token submission must not write the store (leave-blank-keeps-existing)")
	}
}

func TestSaveFieldDurationUnitConversion(t *testing.T) {
	h, cfgPath := writableTestUI(t, newFakeSecretStore())
	// api.cooldown is canonical seconds; entering 2 minutes must store 120.
	rec := postField(t, h, url.Values{"path": {"api.cooldown"}, "value": {"2"}, "unit": {"minutes"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.API.Cooldown != 120 {
		t.Errorf("api.cooldown = %d, want 120 (2 minutes)", cfg.API.Cooldown)
	}
}

func TestSaveFieldProviderEnablementTranslation(t *testing.T) {
	h, cfgPath := writableTestUI(t, newFakeSecretStore())
	// Enable only musixmatch -> the inverse (petitlyrics) is stored disabled.
	rec := postField(t, h, url.Values{"path": {"providers.disabled"}, "value": {"musixmatch"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(cfg.Providers.Disabled) != 1 || cfg.Providers.Disabled[0] != "petitlyrics" {
		t.Errorf("providers.disabled = %v, want [petitlyrics]", cfg.Providers.Disabled)
	}
}

func TestSaveFieldReadOnlyRejected(t *testing.T) {
	h, _ := writableTestUI(t, newFakeSecretStore())
	rec := postField(t, h, url.Values{"path": {"secrets.key_file"}, "value": {"/tmp/x.key"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a read-only field", rec.Code)
	}
}

func TestSaveFieldEnvLockedRejected(t *testing.T) {
	t.Setenv("MXLRC_LOG_LEVEL", "warn")
	h, cfgPath := writableTestUI(t, newFakeSecretStore())
	rec := postField(t, h, url.Values{"path": {"logging.level"}, "value": {"debug"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for an env-locked field", rec.Code)
	}
	// Assert against the raw file: config.Load would overlay the MXLRC_LOG_LEVEL
	// env value and mask whether the file itself was (wrongly) written.
	raw, err := os.ReadFile(cfgPath) //nolint:gosec // G304: test-controlled temp path
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), "debug") {
		t.Errorf("locked field must not write the file; got:\n%s", raw)
	}
}

func TestSaveFieldUnknownRejected(t *testing.T) {
	h, _ := writableTestUI(t, newFakeSecretStore())
	rec := postField(t, h, url.Values{"path": {"api.nope"}, "value": {"x"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field", rec.Code)
	}
}

func TestSaveFieldReadOnlyWhenNoConfigPath(t *testing.T) {
	// No WithConfigPath: the write path is disabled, every save is rejected.
	u := NewUI(config.Config{}, "v0")
	mux := http.NewServeMux()
	u.Register(mux)
	rec := postField(t, mux, url.Values{"path": {"logging.level"}, "value": {"debug"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when no config path is wired", rec.Code)
	}
}

func TestSaveFieldSecretStoreUnavailable(t *testing.T) {
	// Write path wired (config file) but NO secret store: a token save is
	// rejected rather than written to the TOML.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(seedConfigTOML), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath))
	mux := http.NewServeMux()
	u.Register(mux)
	rec := postField(t, mux, url.Values{"path": {"api.token"}, "value": {"tok_x"}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with no secret store", rec.Code)
	}
}

func TestDurationCanonical(t *testing.T) {
	cases := []struct {
		path, value, unit, want string
		wantErr                 bool
	}{
		{"api.cooldown", "2", "minutes", "120", false},
		{"api.cooldown", "30", "seconds", "30", false},
		{"api.miss_backoff_base_hours", "7", "days", "168", false}, // canonical hours
		{"api.cooldown", "5", "fortnights", "", true},              // unknown unit
		{"api.cooldown", "abc", "seconds", "", true},               // non-numeric
		{"api.cooldown", "NaN", "seconds", "", true},               // ParseFloat accepts NaN; must reject
		{"api.cooldown", "Inf", "seconds", "", true},               // ParseFloat accepts Inf; must reject
		{"api.cooldown", "+Inf", "seconds", "", true},              // explicit +Inf
		{"api.cooldown", "-Inf", "seconds", "", true},              // explicit -Inf
	}
	for _, c := range cases {
		got, err := durationCanonical(c.path, c.value, c.unit)
		if c.wantErr {
			if err == nil {
				t.Errorf("durationCanonical(%q,%q,%q) = %q, want error", c.path, c.value, c.unit, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("durationCanonical(%q,%q,%q) error: %v", c.path, c.value, c.unit, err)
		}
		if got != c.want {
			t.Errorf("durationCanonical(%q,%q,%q) = %q, want %q", c.path, c.value, c.unit, got, c.want)
		}
	}
	// Empty unit falls back to the field's display unit.
	if got, err := durationCanonical("api.cooldown", "10", ""); err != nil || got != "10" {
		t.Errorf("empty-unit cooldown = %q (err %v), want 10 (seconds display)", got, err)
	}
}

// errSecretStore is a secrets.Store whose Get always fails, to exercise the
// display path's backend-error branch (a failed read must NOT render as "set"
// and must be logged, never silently swallowed).
type errSecretStore struct{ fakeSecretStore }

func (*errSecretStore) Get(_ context.Context, _ string) (string, bool, error) {
	return "", false, errors.New("backend unavailable")
}

func TestCurrentConfigSecretStoreReadError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(seedConfigTOML), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Capture slog output so we can assert the error was surfaced, not swallowed.
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(&errSecretStore{}))
	cfg := u.currentConfig(context.Background())

	// A failed store read must not be rendered as a present secret.
	if cfg.API.Token == secretPresentSentinel {
		t.Errorf("API.Token = sentinel on store error; failed read must not read as set")
	}
	if len(cfg.Server.WebhookAPIKeys) != 0 {
		t.Errorf("WebhookAPIKeys = %v on store error; failed read must not read as set", cfg.Server.WebhookAPIKeys)
	}
	if !strings.Contains(logBuf.String(), "secret-store read failed") {
		t.Errorf("expected a logged warning about the failed secret-store read, got: %q", logBuf.String())
	}
}

func TestProviderDisabledFromEnabled(t *testing.T) {
	if got := providerDisabledFromEnabled([]string{"musixmatch", "petitlyrics"}); got != "" {
		t.Errorf("both enabled -> disabled = %q, want empty", got)
	}
	if got := providerDisabledFromEnabled(nil); got != "musixmatch,petitlyrics" {
		t.Errorf("none enabled -> disabled = %q, want musixmatch,petitlyrics", got)
	}
	if got := providerDisabledFromEnabled([]string{"petitlyrics"}); got != "musixmatch" {
		t.Errorf("only petitlyrics enabled -> disabled = %q, want musixmatch", got)
	}
}

func TestJoinFormSlice(t *testing.T) {
	if got := joinFormSlice([]string{" a ", "", "b", "  "}); got != "a,b" {
		t.Errorf("joinFormSlice = %q, want a,b", got)
	}
	if got := joinFormSlice(nil); got != "" {
		t.Errorf("joinFormSlice(nil) = %q, want empty", got)
	}
}

func TestSettingsReflectsSavedValueOnReload(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(seedConfigTOML), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	// Save a field in an existing section, and one whose section is ABSENT from
	// the seed ([enrichment]) -> exercises P1 (create section) via the handler.
	if rec := postField(t, mux, url.Values{"path": {"logging.level"}, "value": {"debug"}}); rec.Code != http.StatusOK {
		t.Fatalf("logging.level save = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postField(t, mux, url.Values{"path": {"enrichment.enabled"}, "value": {"true"}}); rec.Code != http.StatusOK {
		t.Fatalf("enrichment.enabled save (absent section) = %d: %s", rec.Code, rec.Body.String())
	}

	// The display path must reflect the on-disk values, not the frozen startup
	// snapshot (#288 P2). Build the view the way the GET handler does.
	view := u.buildSettingsView(u.currentConfig(context.Background()))
	find := func(path string) (string, bool) {
		for _, f := range view.Common {
			if f.Path == path {
				return f.EffectiveValue, true
			}
		}
		for _, sec := range view.Sections {
			for _, f := range sec.Fields {
				if f.Path == path {
					return f.EffectiveValue, true
				}
			}
		}
		return "", false
	}
	if v, ok := find("logging.level"); !ok || v != "debug" {
		t.Errorf("logging.level effective = %q (found=%v), want debug after save", v, ok)
	}
	if v, ok := find("enrichment.enabled"); !ok || v != "true" {
		t.Errorf("enrichment.enabled effective = %q (found=%v), want true after save", v, ok)
	}
}

func TestSaveFieldSemanticValidationRejects(t *testing.T) {
	// Values that parse as the right TYPE but are semantically invalid and would
	// abort boot / break serve must be rejected (400) with the file byte-identical
	// (#288 FIX 1). Each mirrors the loader's own acceptance gate.
	cases := []struct {
		name string
		form url.Values
	}{
		{"bad cidr", url.Values{"path": {"server.trusted_networks.cidrs"}, "value": {"not-a-cidr-garbage"}}},
		{"bad proxy cidr", url.Values{"path": {"server.trusted_networks.trusted_proxies"}, "value": {"10.0.0.0/99"}}},
		{"bad self-signed host", url.Values{"path": {"server.tls.self_signed_hosts"}, "value": {"not a host"}}},
		{"unknown fallback provider", url.Values{"path": {"providers.fallback_order"}, "value": {"bogusprovider"}}},
		{"unknown primary provider", url.Values{"path": {"providers.primary"}, "value": {"nope"}}},
		{"bad listen addr", url.Values{"path": {"server.addr"}, "value": {"this is not a valid addr"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, cfgPath := writableTestUI(t, newFakeSecretStore())
			before, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
			rec := postField(t, h, c.form)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %s", rec.Code, c.name)
			}
			after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
			if !bytes.Equal(before, after) {
				t.Errorf("%s: config file mutated on a rejected save:\n%s", c.name, after)
			}
		})
	}
}

func TestSaveFieldSemanticValidationAccepts(t *testing.T) {
	cases := []struct {
		name  string
		form  url.Values
		check func(config.Config) bool
	}{
		{"good cidr", url.Values{"path": {"server.trusted_networks.cidrs"}, "value": {"192.168.1.0/24"}},
			func(c config.Config) bool {
				return len(c.Server.TrustedNetworks.Cidrs) == 1 && c.Server.TrustedNetworks.Cidrs[0] == "192.168.1.0/24"
			}},
		{"good provider", url.Values{"path": {"providers.primary"}, "value": {"petitlyrics"}},
			func(c config.Config) bool { return c.Providers.Primary == "petitlyrics" }},
		{"good addr", url.Values{"path": {"server.addr"}, "value": {"0.0.0.0:9999"}},
			func(c config.Config) bool { return c.Server.Addr == "0.0.0.0:9999" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, cfgPath := writableTestUI(t, newFakeSecretStore())
			rec := postField(t, h, c.form)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 for %s: %s", rec.Code, c.name, rec.Body.String())
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if !c.check(cfg) {
				t.Errorf("%s: saved value not reflected in reloaded config", c.name)
			}
		})
	}
}

func TestSaveFieldTokenScrubsTOMLToken(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	seed := "[api]\ntoken = \"old_file_token\"\ncooldown = 5\n\n[logging]\nlevel = \"info\"\n"
	if err := os.WriteFile(cfgPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store := newFakeSecretStore()
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(store))
	mux := http.NewServeMux()
	u.Register(mux)

	rec := postField(t, mux, url.Values{"path": {"api.token"}, "value": {"new_store_token"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// The store holds the new token.
	if v, _, _ := store.Get(context.Background(), "musixmatch_token"); v != "new_store_token" {
		t.Errorf("store token = %q, want new_store_token", v)
	}
	// The cleartext token is gone from the TOML; other content is preserved.
	raw, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	got := string(raw)
	if strings.Contains(got, "old_file_token") || strings.Contains(got, "token =") {
		t.Errorf("api.token not scrubbed from TOML:\n%s", got)
	}
	if !strings.Contains(got, "cooldown = 5") || !strings.Contains(got, "[logging]") {
		t.Errorf("scrub disturbed other config content:\n%s", got)
	}
	// With the file token gone, the loader resolves no file token, so the
	// store (lowest precedence) becomes authoritative on next boot.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.API.Token != "" {
		t.Errorf("file still resolves a token after scrub: %q", cfg.API.Token)
	}
}

func TestSaveFieldRejectsDisablingPrimary(t *testing.T) {
	h, cfgPath := writableTestUI(t, newFakeSecretStore())
	before, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	// Enable only petitlyrics -> disables musixmatch, the (default) primary.
	rec := postField(t, h, url.Values{"path": {"providers.disabled"}, "value": {"petitlyrics"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 disabling the primary; body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated rejecting a primary-disable:\n%s", after)
	}
}

func TestSaveFieldRejectsDisablingAllProviders(t *testing.T) {
	h, cfgPath := writableTestUI(t, newFakeSecretStore())
	before, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	// No value entries -> every provider disabled.
	rec := postField(t, h, url.Values{"path": {"providers.disabled"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 disabling all providers; body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated rejecting an all-disabled save:\n%s", after)
	}
}

func TestSaveFieldRejectsPrimarySetToDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[providers]\nprimary = \"musixmatch\"\ndisabled = [\"petitlyrics\"]\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)
	before, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	// Setting primary to a currently-disabled provider would brick boot.
	rec := postField(t, mux, url.Values{"path": {"providers.primary"}, "value": {"petitlyrics"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 setting primary to a disabled provider; body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated rejecting primary-set-to-disabled:\n%s", after)
	}
}

func TestSettingsPrimaryProviderRendersFixed(t *testing.T) {
	// The primary provider's enablement checkbox must render checked + disabled
	// (the UI guard) so the user isn't handed an action that always 400s.
	cfg := config.Config{}
	cfg.Providers.Primary = "musixmatch"
	view := NewUI(cfg, "v0").buildSettingsView(cfg)
	var found bool
	for _, f := range view.Common {
		if f.Path != "providers.disabled" {
			continue
		}
		for _, opt := range f.Options {
			if opt.Value == "musixmatch" {
				found = true
				if !opt.Fixed || !opt.Selected {
					t.Errorf("primary option Fixed=%v Selected=%v, want both true", opt.Fixed, opt.Selected)
				}
			}
			if opt.Value == "petitlyrics" && opt.Fixed {
				t.Error("non-primary provider must not be Fixed")
			}
		}
	}
	if !found {
		t.Fatal("musixmatch option not found in providers.disabled control")
	}
}

// touchFile creates an empty file so ValidatePathExists accepts a cert/key path.
func touchFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("touch %s: %v", name, err)
	}
	return p
}

// pemFile writes a self-signed certificate (or EC key) PEM file and returns its
// path. Used for TLS cert/key fields that now require valid PEM content.
func pemFile(t *testing.T, dir, name string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("pemFile: generate key: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("pemFile: create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("pemFile: marshal key: %v", err)
	}
	var data []byte
	if filepath.Ext(name) == ".key" {
		data = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	} else {
		data = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("pemFile: write %s: %v", name, err)
	}
	return p
}

// tlsTestUI seeds a config file with the given body and returns the handler +
// path. The body must be a VALID config (config.Load is the write-path reload).
func tlsTestUI(t *testing.T, body string) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("seed config must be valid: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)
	return mux, cfgPath
}

func TestSaveFieldRejectsSelfSignedWithCertKey(t *testing.T) {
	dir := t.TempDir()
	cert, key := touchFile(t, dir, "c.pem"), touchFile(t, dir, "k.key")
	cfgPath := filepath.Join(dir, "config.toml")
	body := "[server.tls]\nself_signed = false\ncert_file = \"" + cert + "\"\nkey_file = \"" + key + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	before, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	rec := postField(t, mux, url.Values{"path": {"server.tls.self_signed"}, "value": {"true"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (self_signed mutually exclusive with cert/key); body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected TLS save:\n%s", after)
	}
}

func TestSaveFieldRejectsCertWithoutKey(t *testing.T) {
	dir := t.TempDir()
	cert := touchFile(t, dir, "c.pem")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\naddr = \"127.0.0.1:3876\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	before, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	rec := postField(t, mux, url.Values{"path": {"server.tls.cert_file"}, "value": {cert}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (cert without key); body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected TLS save:\n%s", after)
	}
}

func TestSaveFieldAcceptsSelfSignedAlone(t *testing.T) {
	h, cfgPath := tlsTestUI(t, "[server]\naddr = \"127.0.0.1:3876\"\n")
	rec := postField(t, h, url.Values{"path": {"server.tls.self_signed"}, "value": {"true"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (self_signed alone is valid); body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !cfg.Server.TLS.SelfSigned {
		t.Error("self_signed not persisted")
	}
}

func TestSaveFieldAcceptsCertChangeWhenKeySet(t *testing.T) {
	dir := t.TempDir()
	cert, key := pemFile(t, dir, "c.pem"), pemFile(t, dir, "k.key")
	newCert := pemFile(t, dir, "c2.pem")
	cfgPath := filepath.Join(dir, "config.toml")
	body := "[server.tls]\ncert_file = \"" + cert + "\"\nkey_file = \"" + key + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("seed must be valid: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	// Changing cert while key stays set keeps both set -> valid.
	rec := postField(t, mux, url.Values{"path": {"server.tls.cert_file"}, "value": {newCert}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (cert+key both set); body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Server.TLS.CertFile != newCert {
		t.Errorf("cert_file = %q, want %q", cfg.Server.TLS.CertFile, newCert)
	}
}

func TestSaveFieldConcurrentSingleWriter(t *testing.T) {
	h, cfgPath := writableTestUI(t, newFakeSecretStore())

	// Distinct fields saved concurrently; the single-writer guard must serialize
	// the read-modify-write so no update is lost.
	saves := []url.Values{
		{"path": {"logging.level"}, "value": {"debug"}},
		{"path": {"logging.max_files"}, "value": {"9"}},
		{"path": {"server.scan_interval_seconds"}, "value": {"1200"}, "unit": {"seconds"}},
		{"path": {"api.cooldown"}, "value": {"42"}, "unit": {"seconds"}},
		{"path": {"providers.disabled"}, "value": {"musixmatch"}},
	}
	var wg sync.WaitGroup
	for _, s := range saves {
		wg.Add(1)
		go func(form url.Values) {
			defer wg.Done()
			// Clone so the goroutine's csrf_token set does not race the shared map.
			f := url.Values{}
			for k, v := range form {
				f[k] = append([]string(nil), v...)
			}
			rec := postField(t, h, f)
			if rec.Code != http.StatusOK {
				t.Errorf("concurrent save %v status = %d", form, rec.Code)
			}
		}(s)
	}
	wg.Wait()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("logging.level = %q, want debug", cfg.Logging.Level)
	}
	if cfg.Logging.MaxFiles != 9 {
		t.Errorf("logging.max_files = %d, want 9", cfg.Logging.MaxFiles)
	}
	if cfg.Server.ScanIntervalSeconds != 1200 {
		t.Errorf("server.scan_interval_seconds = %d, want 1200", cfg.Server.ScanIntervalSeconds)
	}
	if cfg.API.Cooldown != 42 {
		t.Errorf("api.cooldown = %d, want 42", cfg.API.Cooldown)
	}
	if len(cfg.Providers.Disabled) != 1 || cfg.Providers.Disabled[0] != "petitlyrics" {
		t.Errorf("providers.disabled = %v, want [petitlyrics]", cfg.Providers.Disabled)
	}
}
