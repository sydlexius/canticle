package webauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// errReader is an io.Reader that always fails, to exercise token/id generation
// error paths.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom: rand failure") }

func TestUserStoreCreateUserRandFailure(t *testing.T) {
	store := NewSQLUserStore(newTestDB(t))
	store.rand = errReader{}
	if _, err := store.CreateUser(context.Background(), "admin", "$argon2id$hash"); err == nil {
		t.Fatal("CreateUser should fail when id generation fails")
	}
}

func TestSessionStoreCreateSessionRandFailure(t *testing.T) {
	sqlDB := newTestDB(t)
	userID := newUserForSession(t, sqlDB)
	store := NewSQLSessionStore(sqlDB)
	store.rand = errReader{}
	if _, err := store.CreateSession(context.Background(), userID, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("CreateSession should fail when token generation fails")
	}
}

func TestSessionStoreCreateSessionEmptyUser(t *testing.T) {
	store := NewSQLSessionStore(newTestDB(t))
	if _, err := store.CreateSession(context.Background(), "", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("CreateSession should reject an empty user id")
	}
}

func TestIsUniqueViolationNonSQLite(t *testing.T) {
	if isUniqueViolation(errors.New("not a sqlite error")) {
		t.Fatal("isUniqueViolation should be false for a non-sqlite error")
	}
}

// failingUserStore and failingSessionStore return a sentinel error from every
// method, to verify the Service propagates store errors rather than swallowing
// them.
var errStore = errors.New("store is down")

type failingUserStore struct{}

func (failingUserStore) CreateUser(context.Context, string, string) (User, error) {
	return User{}, errStore
}
func (failingUserStore) CreateFirstUser(context.Context, string, string) (User, error) {
	return User{}, errStore
}
func (failingUserStore) GetByUsername(context.Context, string) (User, bool, error) {
	return User{}, false, errStore
}
func (failingUserStore) GetByID(context.Context, string) (User, bool, error) {
	return User{}, false, errStore
}
func (failingUserStore) HasUsers(context.Context) (bool, error) { return false, errStore }

type failingSessionStore struct{}

func (failingSessionStore) CreateSession(context.Context, string, time.Time) (string, error) {
	return "", errStore
}
func (failingSessionStore) GetSessionByToken(context.Context, string) (Session, bool, error) {
	return Session{}, false, errStore
}
func (failingSessionStore) DeleteSession(context.Context, string) error { return errStore }
func (failingSessionStore) CleanExpiredSessions(context.Context) (int64, error) {
	return 0, errStore
}

func TestServicePropagatesStoreErrors(t *testing.T) {
	ctx := context.Background()
	svc := NewService(failingUserStore{}, failingSessionStore{})

	if _, err := svc.Setup(ctx, "admin", "supersecret"); !errors.Is(err, errStore) {
		t.Fatalf("Setup error = %v, want errStore", err)
	}
	if _, err := svc.Login(ctx, "admin", "supersecret"); !errors.Is(err, errStore) {
		t.Fatalf("Login error = %v, want errStore", err)
	}
	if _, err := svc.ValidateSession(ctx, "token"); !errors.Is(err, errStore) {
		t.Fatalf("ValidateSession error = %v, want errStore", err)
	}
	if _, err := svc.CleanExpiredSessions(ctx); !errors.Is(err, errStore) {
		t.Fatalf("CleanExpiredSessions error = %v, want errStore", err)
	}
	if _, err := svc.HasUsers(ctx); !errors.Is(err, errStore) {
		t.Fatalf("HasUsers error = %v, want errStore", err)
	}
	if err := svc.Logout(ctx, "token"); !errors.Is(err, errStore) {
		t.Fatalf("Logout error = %v, want errStore", err)
	}
}

// userOnlyFailSession lets Login reach CreateSession (good creds) but then fails
// at the session layer, covering Login's CreateSession error branch.
type okUserStore struct{ user User }

func (s okUserStore) CreateUser(context.Context, string, string) (User, error) { return s.user, nil }
func (s okUserStore) CreateFirstUser(context.Context, string, string) (User, error) {
	return s.user, nil
}
func (s okUserStore) GetByUsername(context.Context, string) (User, bool, error) {
	return s.user, true, nil
}
func (s okUserStore) GetByID(context.Context, string) (User, bool, error) { return s.user, true, nil }
func (s okUserStore) HasUsers(context.Context) (bool, error)              { return true, nil }

func TestServiceLoginSessionCreateError(t *testing.T) {
	ctx := context.Background()
	hash, err := HashPassword("supersecret")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	svc := NewService(okUserStore{user: User{ID: "u1", Username: "admin", PasswordHash: hash}}, failingSessionStore{})
	if _, err := svc.Login(ctx, "admin", "supersecret"); !errors.Is(err, errStore) {
		t.Fatalf("Login error = %v, want errStore from CreateSession", err)
	}
}

func TestStoresReturnErrorsOnClosedDB(t *testing.T) {
	ctx := context.Background()
	sqlDB := newTestDB(t)
	userStore := NewSQLUserStore(sqlDB)
	sessStore := NewSQLSessionStore(sqlDB)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if _, err := userStore.HasUsers(ctx); err == nil {
		t.Fatal("HasUsers should error on a closed db")
	}
	if _, _, err := userStore.GetByUsername(ctx, "admin"); err == nil {
		t.Fatal("GetByUsername should error on a closed db")
	}
	if _, err := userStore.CreateUser(ctx, "admin", "$argon2id$hash"); err == nil {
		t.Fatal("CreateUser should error on a closed db")
	}
	if _, _, err := sessStore.GetSessionByToken(ctx, "tok"); err == nil {
		t.Fatal("GetSessionByToken should error on a closed db")
	}
	if _, err := sessStore.CreateSession(ctx, "u1", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("CreateSession should error on a closed db")
	}
	if err := sessStore.DeleteSession(ctx, "tok"); err == nil {
		t.Fatal("DeleteSession should error on a closed db")
	}
	if _, err := sessStore.CleanExpiredSessions(ctx); err == nil {
		t.Fatal("CleanExpiredSessions should error on a closed db")
	}
}

func TestServiceLoginUnparsableStoredHash(t *testing.T) {
	ctx := context.Background()
	// A user whose stored hash is corrupt: Login must return ErrInvalidCredentials
	// (not leak the parse error) after logging it.
	svc := NewService(okUserStore{user: User{ID: "u1", Username: "admin", PasswordHash: "not-a-phc-string"}}, failingSessionStore{})
	if _, err := svc.Login(ctx, "admin", "supersecret"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login error = %v, want ErrInvalidCredentials", err)
	}
}
