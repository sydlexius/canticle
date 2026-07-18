package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/webauth"
)

// adminSeedPassword is the password every fixture seeds its admin with.
const adminSeedPassword = "initial-password"

// adminTestEnv writes a minimal config pointing at a temp DB, runs migrations,
// and seeds one admin. It returns the config path and the seeded username.
func adminTestEnv(t *testing.T) (cfgPath, user string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath = filepath.Join(dir, "config.toml")

	escaped := strings.ReplaceAll(dbPath, `\`, `\\`)
	if err := os.WriteFile(cfgPath, []byte("[db]\npath = \""+escaped+"\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	svc := webauth.NewService(webauth.NewSQLUserStore(sqlDB), webauth.NewSQLSessionStore(sqlDB))
	if _, err := svc.Setup(ctx, "admin", adminSeedPassword); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return cfgPath, "admin"
}

// openAdminSvc reopens the seeded DB so assertions read committed state rather
// than whatever the command left in memory.
func openAdminSvc(t *testing.T, cfgPath string) (*webauth.Service, func()) {
	t.Helper()
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	// The fixture writes exactly one quoted path.
	parts := strings.SplitN(string(body), `"`, 3)
	if len(parts) < 2 {
		t.Fatalf("could not parse db path from config: %q", body)
	}
	dbPath := strings.ReplaceAll(parts[1], `\\`, `\`)

	sqlDB, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	svc := webauth.NewService(webauth.NewSQLUserStore(sqlDB), webauth.NewSQLSessionStore(sqlDB))
	return svc, func() { _ = sqlDB.Close() }
}

// This is the path that did not exist before #545: rotating an admin password
// without touching the database by hand.
func TestRunAdminSetPassword_RotatesTheCredential(t *testing.T) {
	const newPass = "a-new-password"
	oldPass := adminSeedPassword
	cfgPath, user := adminTestEnv(t)

	var out bytes.Buffer
	code := runAdminSetPassword(t.Context(), &out, strings.NewReader(newPass+"\n"),
		AdminSetPasswordCmd{User: user, ConfigPath: cfgPath})
	if code != 0 {
		t.Fatalf("exit = %d; want 0. output: %s", code, out.String())
	}
	if strings.Contains(out.String(), newPass) {
		t.Error("the new password was echoed to stdout; it must never be printed")
	}

	svc, closeDB := openAdminSvc(t, cfgPath)
	defer closeDB()
	ctx := context.Background()
	if _, err := svc.Login(ctx, user, newPass); err != nil {
		t.Fatalf("login with the rotated password failed: %v", err)
	}
	if _, err := svc.Login(ctx, user, oldPass); err == nil {
		t.Fatal("the old password still works after rotation")
	}
}

func TestRunAdminSetPassword_UnknownUserFails(t *testing.T) {
	cfgPath, _ := adminTestEnv(t)

	var out bytes.Buffer
	code := runAdminSetPassword(t.Context(), &out, strings.NewReader("a-new-password\n"),
		AdminSetPasswordCmd{User: "nobody", ConfigPath: cfgPath})
	if code != 1 {
		t.Fatalf("exit = %d; want 1 for an unknown user", code)
	}
}

func TestRunAdminSetPassword_ShortPasswordRejectedAndOriginalSurvives(t *testing.T) {
	oldPass := adminSeedPassword
	cfgPath, user := adminTestEnv(t)

	var out bytes.Buffer
	code := runAdminSetPassword(t.Context(), &out, strings.NewReader("short\n"),
		AdminSetPasswordCmd{User: user, ConfigPath: cfgPath})
	if code != 1 {
		t.Fatalf("exit = %d; want 1 for a too-short password", code)
	}

	svc, closeDB := openAdminSvc(t, cfgPath)
	defer closeDB()
	if _, err := svc.Login(context.Background(), user, oldPass); err != nil {
		t.Fatalf("a rejected rotation broke the existing password: %v", err)
	}
}

func TestRunAdminSetPassword_BadConfigPathFails(t *testing.T) {
	var out bytes.Buffer
	code := runAdminSetPassword(t.Context(), &out, strings.NewReader("a-new-password\n"),
		AdminSetPasswordCmd{User: "admin", ConfigPath: filepath.Join(t.TempDir(), "nope", "config.toml")})
	if code != 1 {
		t.Fatalf("exit = %d; want 1 when the config cannot be loaded", code)
	}
}

func TestRunAdmin_DispatchesSetPassword(t *testing.T) {
	const newPass = "a-new-password"
	cfgPath, user := adminTestEnv(t)

	var out bytes.Buffer
	code := runAdmin(t.Context(), &out, strings.NewReader(newPass+"\n"),
		AdminCmd{SetPassword: &AdminSetPasswordCmd{User: user, ConfigPath: cfgPath}})
	if code != 0 {
		t.Fatalf("exit = %d; want 0. output: %s", code, out.String())
	}
}
