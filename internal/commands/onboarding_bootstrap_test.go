package commands

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/trustnet"
	"github.com/sydlexius/canticle/internal/webauth"
)

// newBootstrapService opens a migrated temp SQLite DB and returns a webauth
// Service over it (repo rule: real SQLite, not mocks).
func newBootstrapService(t *testing.T) *webauth.Service {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return webauth.NewService(webauth.NewSQLUserStore(sqlDB), webauth.NewSQLSessionStore(sqlDB))
}

// captureLogs redirects the default slog logger into a buffer for the duration
// of the test so log content can be asserted (e.g. the password is never logged).
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// openBootstrapDB opens a migrated temp SQLite DB for the web-auth builder tests.
func openBootstrapDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "buildwebauth.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

func TestBuildWebAuthDisabled(t *testing.T) {
	sqlDB := openBootstrapDB(t)
	cfg := config.Config{} // WebUIEnabled defaults to false
	svc, auth, onboarding, err := buildWebAuth(context.Background(), cfg, sqlDB, nil, trustnet.LoopbackOnly(), "vtest")
	if err != nil {
		t.Fatalf("buildWebAuth: %v", err)
	}
	if svc != nil || auth != nil || onboarding != nil {
		t.Errorf("disabled UI returned non-nil components: svc=%v auth=%v onboarding=%v", svc, auth, onboarding)
	}
}

func TestBuildWebAuthEnabled(t *testing.T) {
	sqlDB := openBootstrapDB(t)
	t.Setenv(envWebAdminUser, "")
	t.Setenv(envWebAdminPass, "")
	cfg := config.Config{}
	cfg.Server.WebUIEnabled = true
	svc, auth, onboarding, err := buildWebAuth(context.Background(), cfg, sqlDB, nil, trustnet.LoopbackOnly(), "vtest")
	if err != nil {
		t.Fatalf("buildWebAuth: %v", err)
	}
	if svc == nil || auth == nil || onboarding == nil {
		t.Fatalf("enabled UI returned a nil component: svc=%v auth=%v onboarding=%v", svc, auth, onboarding)
	}
}

func TestBuildWebAuthBootstrapErrorIsFatal(t *testing.T) {
	sqlDB := openBootstrapDB(t)
	t.Setenv(envWebAdminUser, "admin")
	t.Setenv(envWebAdminPass, "short") // too short -> fatal bootstrap error
	cfg := config.Config{}
	cfg.Server.WebUIEnabled = true
	if _, _, _, err := buildWebAuth(context.Background(), cfg, sqlDB, nil, trustnet.LoopbackOnly(), "vtest"); err == nil {
		t.Fatal("expected a fatal error from a too-short bootstrap password, got nil")
	}
}

func TestSessionSweeper(t *testing.T) {
	svc := newBootstrapService(t)
	// Direct one-shot sweep: no sessions, no error.
	sweepSessions(context.Background(), svc)

	// runSessionSweeper sweeps once then returns when the context is canceled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		runSessionSweeper(ctx, svc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runSessionSweeper did not return after context cancel")
	}
}

func TestBootstrapAdminFromEnvCreates(t *testing.T) {
	svc := newBootstrapService(t)
	t.Setenv(envWebAdminUser, "admin")
	t.Setenv(envWebAdminPass, "correct-horse-battery")

	if err := bootstrapAdminFromEnv(context.Background(), svc); err != nil {
		t.Fatalf("bootstrapAdminFromEnv: %v", err)
	}
	if _, err := svc.Login(context.Background(), "admin", "correct-horse-battery"); err != nil {
		t.Errorf("login with bootstrapped creds failed: %v", err)
	}
}

func TestBootstrapAdminFromEnvIdempotent(t *testing.T) {
	svc := newBootstrapService(t)
	// An admin already exists with different credentials.
	if _, err := svc.Setup(context.Background(), "original", "original-password"); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	t.Setenv(envWebAdminUser, "envadmin")
	t.Setenv(envWebAdminPass, "env-password-1234")

	if err := bootstrapAdminFromEnv(context.Background(), svc); err != nil {
		t.Fatalf("bootstrapAdminFromEnv: %v", err)
	}
	// The original admin is intact and the env admin was NOT created (no overwrite).
	if _, err := svc.Login(context.Background(), "original", "original-password"); err != nil {
		t.Errorf("original admin broken by bootstrap: %v", err)
	}
	if _, err := svc.Login(context.Background(), "envadmin", "env-password-1234"); err == nil {
		t.Error("env admin was created despite an existing admin (overwrite)")
	}
}

func TestBootstrapAdminFromEnvPartialVarsWarns(t *testing.T) {
	cases := []struct {
		name, user, pass string
	}{
		{"only user", "admin", ""},
		{"only password", "", "correct-horse-battery"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newBootstrapService(t)
			buf := captureLogs(t)
			t.Setenv(envWebAdminUser, tc.user)
			t.Setenv(envWebAdminPass, tc.pass)

			if err := bootstrapAdminFromEnv(context.Background(), svc); err != nil {
				t.Fatalf("bootstrapAdminFromEnv: %v", err)
			}
			if has, _ := svc.HasUsers(context.Background()); has {
				t.Error("partial env vars created an admin")
			}
			if !strings.Contains(buf.String(), "both") {
				t.Errorf("expected a warning that both vars are required; logs: %q", buf.String())
			}
		})
	}
}

func TestBootstrapAdminFromEnvTooShortFailsLoud(t *testing.T) {
	svc := newBootstrapService(t)
	t.Setenv(envWebAdminUser, "admin")
	t.Setenv(envWebAdminPass, "short") // < MinPasswordLength

	err := bootstrapAdminFromEnv(context.Background(), svc)
	if err == nil {
		t.Fatal("expected a fatal error for a too-short env password, got nil")
	}
	if has, _ := svc.HasUsers(context.Background()); has {
		t.Error("a too-short password still created an admin")
	}
}

func TestBootstrapAdminFromEnvNoVarsNoop(t *testing.T) {
	svc := newBootstrapService(t)
	t.Setenv(envWebAdminUser, "")
	t.Setenv(envWebAdminPass, "")

	if err := bootstrapAdminFromEnv(context.Background(), svc); err != nil {
		t.Fatalf("bootstrapAdminFromEnv: %v", err)
	}
	if has, _ := svc.HasUsers(context.Background()); has {
		t.Error("no env vars set but an admin was created")
	}
}

func TestBootstrapAdminFromEnvNeverLogsPassword(t *testing.T) {
	const secretPass = "zxq-never-log-this-9f3"
	svc := newBootstrapService(t)
	buf := captureLogs(t)
	t.Setenv(envWebAdminUser, "admin")
	t.Setenv(envWebAdminPass, secretPass)

	if err := bootstrapAdminFromEnv(context.Background(), svc); err != nil {
		t.Fatalf("bootstrapAdminFromEnv: %v", err)
	}
	// Sanity: it logged a success line naming the user.
	if !strings.Contains(buf.String(), "bootstrapped web admin") {
		t.Errorf("expected a success log line; logs: %q", buf.String())
	}
	if strings.Contains(buf.String(), secretPass) {
		t.Error("the admin password leaked into the logs")
	}
}
