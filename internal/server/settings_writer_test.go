package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/secrets"
	"github.com/sydlexius/canticle/internal/web"
)

// fakeSecretStore is an in-memory secrets.Store for the settings-writer test.
type fakeSecretStore struct {
	vals map[string]string
}

func (f *fakeSecretStore) Set(_ context.Context, name, plaintext string) error {
	f.vals[name] = plaintext
	return nil
}
func (f *fakeSecretStore) Get(_ context.Context, name string) (string, bool, error) {
	v, ok := f.vals[name]
	return v, ok, nil
}
func (f *fakeSecretStore) Delete(_ context.Context, name string) error {
	delete(f.vals, name)
	return nil
}
func (f *fakeSecretStore) List(_ context.Context) ([]secrets.SecretInfo, error) { return nil, nil }

// TestWithSettingsWriterSavesField mounts the web UI with the settings write
// path through the real handler (WithWebUI + WithSettingsWriter) and confirms a
// CSRF-valid POST /settings/field is routed end-to-end into config.ApplyChanges,
// updating the on-disk config the daemon would reload.
func TestWithSettingsWriterSavesField(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[logging]\nlevel = \"info\"\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	store := &fakeSecretStore{vals: map[string]string{}}

	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithWebUI(config.Config{}, "vtest"),
		WithSettingsWriter(cfgPath, store))

	const token = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	form := url.Values{"csrf_token": {token}, "path": {"logging.level"}, "value": {"warn"}}
	req := httptest.NewRequest(http.MethodPost, "/settings/field", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: web.CSRFCookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/field status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Logging.Level != "warn" {
		t.Errorf("logging.level = %q, want warn (write did not reach the file)", cfg.Logging.Level)
	}
}
