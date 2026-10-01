package web

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// postSection issues a section save POST with a valid double-submit CSRF token.
// Each pair is sent as a repeated "path" plus its value under a form key equal
// to the path, matching the wire format settings.js builds for a save group.
func postSection(t *testing.T, h http.Handler, pairs [][2]string) *httptest.ResponseRecorder {
	t.Helper()
	return postSectionAccept(t, h, pairs, "")
}

// postSectionAccept is postSection with an Accept header, for the JSON error shape.
func postSectionAccept(t *testing.T, h http.Handler, pairs [][2]string, accept string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("csrf_token", testCSRFToken)
	for _, p := range pairs {
		form.Add("path", p[0])
		form.Set(p[0], p[1])
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/section", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: testCSRFToken})
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSaveSectionBootstrapsCertKeyPairFromEmpty(t *testing.T) {
	dir := t.TempDir()
	cert, key := pemFile(t, dir, "c.pem"), pemFile(t, dir, "k.key")
	cfgPath := filepath.Join(dir, "config.toml")
	// No [server.tls] at all: neither cert nor key is set. A single-field save
	// could never get here (cert alone 400s on the blank key); the section save
	// writes both at once.
	if err := os.WriteFile(cfgPath, []byte("[server]\naddr = \"127.0.0.1:3876\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	rec := postSection(t, mux, [][2]string{
		{"server.tls.cert_file", cert},
		{"server.tls.key_file", key},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (paired cert+key bootstrap); body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Server.TLS.CertFile != cert || cfg.Server.TLS.KeyFile != key {
		t.Errorf("cert/key = %q/%q, want %q/%q", cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile, cert, key)
	}
}

func TestSaveSectionCertOnlyRejected(t *testing.T) {
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
	// Cert set, key blank -> the "set together" invariant still fails.
	rec := postSection(t, mux, [][2]string{
		{"server.tls.cert_file", cert},
		{"server.tls.key_file", ""},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (cert without key); body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected section save:\n%s", after)
	}
}

func TestSaveSectionKeyOnlyRejected(t *testing.T) {
	dir := t.TempDir()
	key := touchFile(t, dir, "k.key")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\naddr = \"127.0.0.1:3876\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	before, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	rec := postSection(t, mux, [][2]string{
		{"server.tls.cert_file", ""},
		{"server.tls.key_file", key},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (key without cert); body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected section save:\n%s", after)
	}
}

func TestSaveSectionChangesCertKeyWhenBothSet(t *testing.T) {
	dir := t.TempDir()
	cert, key := pemFile(t, dir, "c.pem"), pemFile(t, dir, "k.key")
	newCert, newKey := pemFile(t, dir, "c2.pem"), pemFile(t, dir, "k2.key")
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

	rec := postSection(t, mux, [][2]string{
		{"server.tls.cert_file", newCert},
		{"server.tls.key_file", newKey},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (both stay set); body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Server.TLS.CertFile != newCert || cfg.Server.TLS.KeyFile != newKey {
		t.Errorf("cert/key = %q/%q, want %q/%q", cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile, newCert, newKey)
	}
}

func TestSaveSectionRejectsSelfSignedConflict(t *testing.T) {
	dir := t.TempDir()
	cert, key := touchFile(t, dir, "c.pem"), touchFile(t, dir, "k.key")
	cfgPath := filepath.Join(dir, "config.toml")
	// self_signed already on: a cert+key pair is mutually exclusive with it.
	if err := os.WriteFile(cfgPath, []byte("[server.tls]\nself_signed = true\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("seed must be valid: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	before, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	rec := postSection(t, mux, [][2]string{
		{"server.tls.cert_file", cert},
		{"server.tls.key_file", key},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (self_signed conflicts with cert/key); body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected section save:\n%s", after)
	}
}

func TestSaveSectionRejectsNonexistentCertPath(t *testing.T) {
	dir := t.TempDir()
	key := touchFile(t, dir, "k.key")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\naddr = \"127.0.0.1:3876\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	before, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	rec := postSection(t, mux, [][2]string{
		{"server.tls.cert_file", filepath.Join(dir, "does-not-exist.pem")},
		{"server.tls.key_file", key},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (cert path does not exist); body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected section save:\n%s", after)
	}
}

func TestSaveSectionCSRFRejected(t *testing.T) {
	dir := t.TempDir()
	cert, key := touchFile(t, dir, "c.pem"), touchFile(t, dir, "k.key")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\naddr = \"127.0.0.1:3876\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	// No CSRF cookie or form token.
	form := url.Values{}
	form.Add("path", "server.tls.cert_file")
	form.Set("server.tls.cert_file", cert)
	form.Add("path", "server.tls.key_file")
	form.Set("server.tls.key_file", key)
	req := httptest.NewRequest(http.MethodPost, "/settings/section", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF)", rec.Code)
	}
}

func TestSaveSectionRejectsSecretField(t *testing.T) {
	h, _ := writableTestUI(t, newFakeSecretStore())
	// A secret must never be routed through the section save into the TOML.
	rec := postSection(t, h, [][2]string{{"api.token", "tok_x"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (secret not allowed in section save); body=%s", rec.Code, rec.Body.String())
	}
}

func TestSaveSectionUnknownFieldRejected(t *testing.T) {
	h, _ := writableTestUI(t, newFakeSecretStore())
	rec := postSection(t, h, [][2]string{{"api.nope", "x"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field)", rec.Code)
	}
}

func TestSaveSectionNoFieldsRejected(t *testing.T) {
	h, _ := writableTestUI(t, newFakeSecretStore())
	rec := postSection(t, h, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (no fields)", rec.Code)
	}
}

func TestSaveSectionEnvLockedRejected(t *testing.T) {
	dir := t.TempDir()
	cert, key := touchFile(t, dir, "c.pem"), touchFile(t, dir, "k.key")
	t.Setenv("MXLRC_TLS_CERT_FILE", cert) // cert_file now env-locked
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\naddr = \"127.0.0.1:3876\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	rec := postSection(t, mux, [][2]string{
		{"server.tls.cert_file", cert},
		{"server.tls.key_file", key},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (env-locked field); body=%s", rec.Code, rec.Body.String())
	}
}

func TestSaveSectionReadOnlyWhenNoConfigPath(t *testing.T) {
	u := NewUI(config.Config{}, "v0")
	mux := http.NewServeMux()
	u.Register(mux)
	rec := postSection(t, mux, [][2]string{{"server.tls.cert_file", "/tmp/c.pem"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when no config path is wired", rec.Code)
	}
}

func TestSaveSectionRejectsProviderInvariant(t *testing.T) {
	// The section endpoint is generic; it must still enforce the provider
	// invariant against the resulting state (disabling the primary bricks boot).
	h, cfgPath := writableTestUI(t, newFakeSecretStore())
	before, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	rec := postSection(t, h, [][2]string{{"providers.disabled", "musixmatch"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (disabling the primary); body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated rejecting a primary-disable:\n%s", after)
	}
}

func TestSaveSectionRejectsReadOnlyField(t *testing.T) {
	h, _ := writableTestUI(t, newFakeSecretStore())
	// secrets.key_file is Editable=false: the writer must never rewrite it.
	rec := postSection(t, h, [][2]string{{"secrets.key_file", "/tmp/x.key"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a read-only field; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSaveSectionRejectsDuplicateField(t *testing.T) {
	dir := t.TempDir()
	cert := touchFile(t, dir, "c.pem")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\naddr = \"127.0.0.1:3876\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	// Same path twice in one batch is rejected (a forged/buggy POST).
	form := url.Values{}
	form.Set("csrf_token", testCSRFToken)
	form.Add("path", "server.tls.cert_file")
	form.Add("path", "server.tls.cert_file")
	form.Set("server.tls.cert_file", cert)
	req := httptest.NewRequest(http.MethodPost, "/settings/section", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: testCSRFToken})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (duplicate field); body=%s", rec.Code, rec.Body.String())
	}
}

func TestSaveSectionSelfSignedAndPrimaryViaSection(t *testing.T) {
	// Exercises the self_signed and providers.primary folds in the resulting-state
	// invariant checks: a section save touching each valid field succeeds.
	h, cfgPath := writableTestUI(t, newFakeSecretStore())
	if rec := postSection(t, h, [][2]string{{"server.tls.self_signed", "true"}}); rec.Code != http.StatusOK {
		t.Fatalf("self_signed section status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := postSection(t, h, [][2]string{{"providers.primary", "petitlyrics"}}); rec.Code != http.StatusOK {
		t.Fatalf("primary section status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !cfg.Server.TLS.SelfSigned {
		t.Error("self_signed not persisted via section save")
	}
	if cfg.Providers.Primary != "petitlyrics" {
		t.Errorf("providers.primary = %q, want petitlyrics", cfg.Providers.Primary)
	}
}

// TestSaveSectionSettingsFieldCarriesSaveGroup asserts the read path tags the
// cert/key cards with the shared save group so settings.js routes them to the
// section endpoint, and that no other field carries it.
func TestSaveSectionSettingsFieldCarriesSaveGroup(t *testing.T) {
	view := NewUI(config.Config{}, "v0").buildSettingsView(config.Config{})
	groups := map[string]string{}
	for _, sec := range view.Sections {
		for _, f := range sec.Fields {
			if f.SaveGroup != "" {
				groups[f.Path] = f.SaveGroup
			}
		}
	}
	for _, f := range view.Common {
		if f.SaveGroup != "" {
			groups[f.Path] = f.SaveGroup
		}
	}
	if groups["server.tls.cert_file"] != tlsCertKeySaveGroup {
		t.Errorf("cert_file SaveGroup = %q, want %q", groups["server.tls.cert_file"], tlsCertKeySaveGroup)
	}
	if groups["server.tls.key_file"] != tlsCertKeySaveGroup {
		t.Errorf("key_file SaveGroup = %q, want %q", groups["server.tls.key_file"], tlsCertKeySaveGroup)
	}
	if len(groups) != 2 {
		t.Errorf("unexpected fields carry a save group: %v", groups)
	}
}

func TestSaveSectionReportsAppliedPerPath(t *testing.T) {
	dir := t.TempDir()
	cert, key := pemFile(t, dir, "c.pem"), pemFile(t, dir, "k.key")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\naddr = \"127.0.0.1:3876\"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)

	rec := postSection(t, mux, [][2]string{
		{"server.tls.cert_file", cert},
		{"server.tls.key_file", key},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeSaveResponse(t, rec)
	want := map[string]string{"server.tls.cert_file": "restart", "server.tls.key_file": "restart"}
	if got.Status != "saved" || !maps.Equal(got.Applied, want) {
		t.Errorf("response = %+v, want status saved applied %v", got, want)
	}
}

// postRejectedBatch posts a batch expected to be rejected, asking for JSON, and
// returns the decoded per-field errors after asserting the file is byte-identical.
func postRejectedBatch(t *testing.T, pairs [][2]string) sectionErrorResponse {
	t.Helper()
	h, cfgPath := writableTestUI(t, newFakeSecretStore())
	before, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	rec := postSectionAccept(t, h, pairs, "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected batch:\n%s", after)
	}
	var got sectionErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if got.Status != "error" {
		t.Errorf("status field = %q, want error", got.Status)
	}
	return got
}

func TestSaveSectionBatchCollectsEveryFieldError(t *testing.T) {
	got := postRejectedBatch(t, [][2]string{
		{"logging.level", "loud"},
		{"providers.mode", "bogus"},
	})
	for _, p := range []string{"logging.level", "providers.mode"} {
		if got.Errors[p] == "" {
			t.Errorf("no error for %s; errors=%v", p, got.Errors)
		}
	}
	if len(got.Errors) != 2 {
		t.Errorf("errors = %v, want exactly the two invalid fields", got.Errors)
	}
}

func TestSaveSectionBatchValidPlusInvalidWritesNothing(t *testing.T) {
	got := postRejectedBatch(t, [][2]string{
		{"logging.level", "debug"},
		{"providers.mode", "bogus"},
	})
	if got.Errors["providers.mode"] == "" || got.Errors["logging.level"] != "" {
		t.Errorf("errors = %v, want only providers.mode", got.Errors)
	}
}

func TestSaveSectionBatchCrossFieldViolationRejectsEverything(t *testing.T) {
	// frequency=daily without "at" is individually valid but incomplete together;
	// the unrelated valid logging.level must not land either.
	got := postRejectedBatch(t, [][2]string{
		{"logging.level", "debug"},
		{"server.scan_schedule.frequency", "daily"},
	})
	if got.Errors["server.scan_schedule.frequency"] == "" {
		t.Errorf("errors = %v, want the schedule field flagged", got.Errors)
	}
	if got.Errors["logging.level"] != "" {
		t.Errorf("valid unrelated field flagged: %v", got.Errors)
	}
}

func TestSaveSectionBatchHalfFailedPairNotDoubleReported(t *testing.T) {
	// The cert is valid and survives field validation; the key fails it. The pair
	// check must be skipped, so the key carries only its own error and the
	// surviving cert is not flagged with a misleading "set together" message.
	cert := pemFile(t, t.TempDir(), "c.pem")
	got := postRejectedBatch(t, [][2]string{
		{"server.tls.cert_file", cert},
		{"server.tls.key_file", "/nonexistent/k.key"},
	})
	if len(got.Errors) != 1 || got.Errors["server.tls.key_file"] == "" {
		t.Fatalf("errors = %v, want exactly one entry keyed server.tls.key_file", got.Errors)
	}
	if _, ok := got.Errors["server.tls.cert_file"]; ok {
		t.Errorf("valid cert_file flagged: %v", got.Errors)
	}
	if strings.Contains(got.Errors["server.tls.key_file"], "together") {
		t.Errorf("pair-invariant error on a field-level failure: %q", got.Errors["server.tls.key_file"])
	}
}

func TestSaveSectionBatchConflictWinsRegardlessOfOrder(t *testing.T) {
	orders := [][][2]string{
		{{"providers.mode", "bogus"}, {"secrets.key_file", "/tmp/x.key"}},
		{{"secrets.key_file", "/tmp/x.key"}, {"providers.mode", "bogus"}},
	}
	for _, pairs := range orders {
		h, _ := writableTestUI(t, newFakeSecretStore())
		rec := postSectionAccept(t, h, pairs, "application/json")
		if rec.Code != http.StatusConflict {
			t.Errorf("order %v: status = %d, want 409; body=%s", pairs, rec.Code, rec.Body.String())
		}
	}
}

func TestSaveSectionPlainSummaryDedupesSharedMessage(t *testing.T) {
	// self_signed with a cert/key pair is one cross-field conflict reported on
	// every member; the plain-text summary states the message once.
	dir := t.TempDir()
	cert, key := pemFile(t, dir, "c.pem"), pemFile(t, dir, "k.key")
	h, _ := writableTestUI(t, newFakeSecretStore())
	rec := postSection(t, h, [][2]string{
		{"server.tls.self_signed", "true"},
		{"server.tls.cert_file", cert},
		{"server.tls.key_file", key},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), "mutually exclusive"); n != 1 {
		t.Errorf("shared message appears %d times, want 1; body=%s", n, rec.Body.String())
	}
}

func TestSaveSectionBatchAllValidWritesAllInOneSave(t *testing.T) {
	h, cfgPath := writableTestUI(t, newFakeSecretStore())
	rec := postSection(t, h, [][2]string{
		{"logging.level", "debug"},
		{"providers.mode", "parallel"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Logging.Level != "debug" || cfg.Providers.Mode != "parallel" {
		t.Errorf("level/mode = %q/%q, want debug/parallel", cfg.Logging.Level, cfg.Providers.Mode)
	}
	if got := decodeSaveResponse(t, rec); len(got.Applied) != 2 {
		t.Errorf("applied = %v, want both paths", got.Applied)
	}
}

func TestSaveSectionRejectionPlainTextWithoutAccept(t *testing.T) {
	// Today's settings.js sends no Accept header and renders the body verbatim.
	h, _ := writableTestUI(t, newFakeSecretStore())
	rec := postSection(t, h, [][2]string{{"providers.mode", "bogus"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain for a client not asking for JSON", ct)
	}
}
