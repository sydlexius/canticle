package webauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	sqlDB := newTestDB(t)
	svc := NewService(NewSQLUserStore(sqlDB), NewSQLSessionStore(sqlDB))
	return svc, sqlDB
}

func TestNewServicePanicsOnNilStore(t *testing.T) {
	sqlDB := newTestDB(t)
	us := NewSQLUserStore(sqlDB)
	ss := NewSQLSessionStore(sqlDB)

	cases := []struct {
		name     string
		users    UserStore
		sessions SessionStore
	}{
		{"nil users", nil, ss},
		{"nil sessions", us, nil},
		{"both nil", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("NewService did not panic on nil store")
				}
			}()
			NewService(tc.users, tc.sessions)
		})
	}
}

func TestServiceSetupCreatesAdmin(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)

	has, err := svc.HasUsers(ctx)
	if err != nil || has {
		t.Fatalf("HasUsers before setup = (%v, %v), want (false, nil)", has, err)
	}

	user, err := svc.Setup(ctx, "admin", "supersecret")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if user.ID == "" || user.Username != "admin" {
		t.Fatalf("Setup returned %+v", user)
	}
	if user.PasswordHash == "supersecret" {
		t.Fatal("Setup stored the password in plaintext")
	}

	has, err = svc.HasUsers(ctx)
	if err != nil || !has {
		t.Fatalf("HasUsers after setup = (%v, %v), want (true, nil)", has, err)
	}
}

func TestServiceSetupRejectsSecond(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)

	if _, err := svc.Setup(ctx, "admin", "supersecret"); err != nil {
		t.Fatalf("first Setup: %v", err)
	}
	_, err := svc.Setup(ctx, "other", "anothersecret")
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("second Setup error = %v, want ErrUserExists", err)
	}
}

func TestServiceSetupConcurrentSingleAdmin(t *testing.T) {
	ctx := context.Background()
	svc, sqlDB := newTestService(t)

	// Fire many first-run setups at once, each with a DISTINCT username (the case
	// the username UNIQUE constraint does NOT catch). Exactly one must win; the
	// rest must be rejected. On the pre-fix HasUsers+CreateUser code this created
	// multiple admins (TOCTOU privilege escalation); the atomic CreateFirstUser
	// closes that window.
	const n = 12
	var wg sync.WaitGroup
	results := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.Setup(ctx, fmt.Sprintf("admin%d", i), "supersecret")
			results[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	successes := 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrUserExists):
		default:
			t.Fatalf("unexpected concurrent Setup error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent Setup produced %d admins, want exactly 1", successes)
	}

	var count int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("users table has %d rows after concurrent setup, want 1", count)
	}
}

func TestServiceSetupValidation(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)

	if _, err := svc.Setup(ctx, "admin", "short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("short password error = %v, want ErrPasswordTooShort", err)
	}
	if _, err := svc.Setup(ctx, "  ", "supersecret"); err == nil {
		t.Fatal("blank username should be rejected")
	}
}

func TestServiceLoginSuccess(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)

	admin, err := svc.Setup(ctx, "admin", "supersecret")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	token, err := svc.Login(ctx, "admin", "supersecret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if token == "" {
		t.Fatal("Login returned an empty token")
	}

	user, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if user == nil || user.ID != admin.ID {
		t.Fatalf("ValidateSession returned %+v, want id %q", user, admin.ID)
	}
}

func TestServiceLoginCaseInsensitiveUsername(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	if _, err := svc.Setup(ctx, "Admin", "supersecret"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if _, err := svc.Login(ctx, "ADMIN", "supersecret"); err != nil {
		t.Fatalf("Login with different-case username: %v", err)
	}
}

func TestServiceLoginWrongPassword(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", "supersecret"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	_, err := svc.Login(ctx, "admin", "wrongpassword")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login wrong password error = %v, want ErrInvalidCredentials", err)
	}
}

func TestServiceLoginUnknownUser(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", "supersecret"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	// Unknown user returns the SAME error as a wrong password (enumeration-safe).
	_, err := svc.Login(ctx, "ghost", "supersecret")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login unknown user error = %v, want ErrInvalidCredentials", err)
	}
}

func TestServiceValidateSessionInvalid(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)

	if _, err := svc.ValidateSession(ctx, ""); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("ValidateSession(empty) error = %v, want ErrInvalidSession", err)
	}
	if _, err := svc.ValidateSession(ctx, "bogus-token"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("ValidateSession(bogus) error = %v, want ErrInvalidSession", err)
	}
}

func TestServiceLogout(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", "supersecret"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	token, err := svc.Login(ctx, "admin", "supersecret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := svc.Logout(ctx, token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.ValidateSession(ctx, token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("session still valid after logout: %v", err)
	}
	// Logging out an already-cleared token is a no-op.
	if err := svc.Logout(ctx, token); err != nil {
		t.Fatalf("double Logout: %v", err)
	}
	if err := svc.Logout(ctx, ""); err != nil {
		t.Fatalf("Logout(empty): %v", err)
	}
}

func TestServiceSessionExpiry(t *testing.T) {
	ctx := context.Background()
	sqlDB := newTestDB(t)
	clock := &fakeClock{t: time.Now()}

	sessStore := NewSQLSessionStore(sqlDB)
	sessStore.now = clock.Now // white-box: drive the store's expiry checks off the fake clock
	svc := NewService(NewSQLUserStore(sqlDB), sessStore,
		WithClock(clock.Now), WithSessionTTL(time.Hour))

	if _, err := svc.Setup(ctx, "admin", "supersecret"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	token, err := svc.Login(ctx, "admin", "supersecret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Still valid within the TTL window.
	if _, err := svc.ValidateSession(ctx, token); err != nil {
		t.Fatalf("ValidateSession within TTL: %v", err)
	}

	// Advance past the TTL: the session must be rejected.
	clock.Advance(2 * time.Hour)
	if _, err := svc.ValidateSession(ctx, token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("expired session error = %v, want ErrInvalidSession", err)
	}

	// And the sweeper removes it.
	removed, err := svc.CleanExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("CleanExpiredSessions: %v", err)
	}
	if removed != 1 {
		t.Fatalf("CleanExpiredSessions removed %d, want 1", removed)
	}
}

func TestServiceValidateSessionOrphanedUser(t *testing.T) {
	ctx := context.Background()
	svc, sqlDB := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", "supersecret"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	token, err := svc.Login(ctx, "admin", "supersecret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	// Deleting the user cascades to its sessions (ON DELETE CASCADE); the token
	// must then be invalid rather than resolving to a missing user.
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM users`); err != nil {
		t.Fatalf("delete users: %v", err)
	}
	if _, err := svc.ValidateSession(ctx, token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("orphaned session error = %v, want ErrInvalidSession", err)
	}
}
