package webauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/dbtest"
)

// openSplit opens one migrated file as the single-connection writer and a
// query-only read pool, the way serve mode does (#1652).
func openSplit(t *testing.T) (svcSplit, svcWriterOnly *Service, hold func()) {
	t.Helper()
	ctx := context.Background()
	path := dbtest.Path(t)
	w, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	p, err := db.OpenReadPool(ctx, path, 2)
	if err != nil {
		t.Fatalf("db.OpenReadPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	svcSplit = NewService(NewSQLUserStore(w).WithReadDB(p), NewSQLSessionStore(w).WithReadDB(p))
	svcWriterOnly = NewService(NewSQLUserStore(w), NewSQLSessionStore(w))
	hold = func() {
		t.Helper()
		conn, err := w.Conn(ctx)
		if err != nil {
			t.Fatalf("hold the writer's connection: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	return svcSplit, svcWriterOnly, hold
}

// TestSplitService_LoginThenValidateThroughPool pins read-after-write across the
// split: a session the writer just committed is visible to the very next pooled
// lookup, and a logout is too.
func TestSplitService_LoginThenValidateThroughPool(t *testing.T) {
	svc, _, _ := openSplit(t)
	ctx := context.Background()
	if _, err := svc.Setup(ctx, "admin", "correct-horse-battery"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	has, err := svc.HasUsers(ctx)
	if err != nil || !has {
		t.Fatalf("HasUsers after Setup = %v, %v; want true", has, err)
	}
	for i := 0; i < 5; i++ {
		token, err := svc.Login(ctx, "admin", "correct-horse-battery")
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
		u, err := svc.ValidateSession(ctx, token)
		if err != nil || u.Username != "admin" {
			t.Fatalf("ValidateSession right after Login = %+v, %v; want admin", u, err)
		}
		if err := svc.Logout(ctx, token); err != nil {
			t.Fatalf("Logout: %v", err)
		}
		if _, err := svc.ValidateSession(ctx, token); !errors.Is(err, ErrInvalidSession) {
			t.Fatalf("ValidateSession after Logout = %v; want ErrInvalidSession", err)
		}
	}
}

// TestSplitService_ValidateSessionPastHeldWriter is the #1652 regression: with
// the writer's single connection held (a long worker transaction), a session
// check through the split service returns at once, while the same check through
// writer-only stores blocks until its deadline. The control is what makes the
// first half meaningful: it proves the hold really starves the writer.
func TestSplitService_ValidateSessionPastHeldWriter(t *testing.T) {
	split, writerOnly, hold := openSplit(t)
	ctx := context.Background()
	if _, err := split.Setup(ctx, "admin", "correct-horse-battery"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	token, err := split.Login(ctx, "admin", "correct-horse-battery")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	hold()

	const budget = 2 * time.Second
	check := func(svc *Service) (time.Duration, error) {
		cctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		start := time.Now()
		_, err := svc.ValidateSession(cctx, token)
		return time.Since(start), err
	}

	took, err := check(split)
	if err != nil {
		t.Fatalf("split ValidateSession with the writer held: %v", err)
	}
	if took > budget/4 {
		t.Errorf("split ValidateSession took %v with the writer held; want well under %v", took, budget)
	}
	hasUsers := func(svc *Service) error {
		cctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		_, err := svc.HasUsers(cctx)
		return err
	}
	if err := hasUsers(split); err != nil {
		t.Errorf("split HasUsers with the writer held: %v", err)
	}

	if _, err := check(writerOnly); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("control: writer-only ValidateSession with the writer held = %v; want a deadline error (the hold must starve the writer)", err)
	}
	if err := hasUsers(writerOnly); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("control: writer-only HasUsers with the writer held = %v; want a deadline error", err)
	}
}
