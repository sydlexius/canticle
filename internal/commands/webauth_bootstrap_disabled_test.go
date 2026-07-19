package commands

import (
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// TestWebAuthBootstrapVarsIgnoredWhenWebUIDisabled covers a silent no-op of the
// same family as the one that shipped in v1.20.0.
//
// buildWebAuth returns early when the web UI is disabled, before
// bootstrapAdminFromEnv is ever reached. So an operator who sets both
// MXLRC_WEBAUTH_ADMIN_* variables but leaves web_ui_enabled at its default gets
// no admin account, no error, and no explanation: the service starts healthy and
// the credentials they supplied did nothing.
//
// That is the same shape as editing the password variable after first run and
// believing a rotation happened. The fix is not to bootstrap anyway (an admin
// account is meaningless with no UI to sign into) but to say plainly that the
// variables are being ignored and why.
func TestWebAuthBootstrapVarsIgnoredWhenWebUIDisabled(t *testing.T) {
	t.Setenv(envWebAdminUser, "admin")
	t.Setenv(envWebAdminPass, "a-long-enough-password")
	buf := captureLogs(t)

	cfg := config.Config{}
	cfg.Server.WebUIEnabled = false

	svc, auth, onboarding, err := buildWebAuth(t.Context(), cfg, nil, nil, nil, "test")
	if err != nil {
		t.Fatalf("buildWebAuth: %v", err)
	}
	if svc != nil || auth != nil || onboarding != nil {
		t.Fatal("web UI disabled must still yield all-nil components")
	}

	got := buf.String()
	if !strings.Contains(got, envWebAdminPass) {
		t.Errorf("log does not name the ignored variable %s:\n%s", envWebAdminPass, got)
	}
	if !strings.Contains(got, "web_ui_enabled") {
		t.Errorf("log does not name the setting to change:\n%s", got)
	}
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("ignored credentials must be reported at WARN, not buried at INFO:\n%s", got)
	}
}

// The warning must not fire for the overwhelmingly common case of running with
// the web UI off and no bootstrap variables set. A warning on every startup
// would train operators to ignore it.
func TestWebAuthNoWarningWhenBootstrapVarsUnset(t *testing.T) {
	t.Setenv(envWebAdminUser, "")
	t.Setenv(envWebAdminPass, "")
	buf := captureLogs(t)

	cfg := config.Config{}
	cfg.Server.WebUIEnabled = false

	if _, _, _, err := buildWebAuth(t.Context(), cfg, nil, nil, nil, "test"); err != nil {
		t.Fatalf("buildWebAuth: %v", err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("warned with no bootstrap vars set:\n%s", buf.String())
	}
}
