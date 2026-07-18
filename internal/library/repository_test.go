package library_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

func TestAddGetListUpdateRemove(t *testing.T) {
	ctx := context.Background()
	repo := library.New(openTestDB(t))

	added, err := repo.Add(ctx, "/music/rock", "Rock", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.ID == 0 {
		t.Fatal("Add returned zero ID")
	}
	if added.Path != "/music/rock" || added.Name != "Rock" {
		t.Fatalf("Add returned %+v", added)
	}

	got, err := repo.Get(ctx, added.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != added {
		t.Fatalf("Get got %+v; want %+v", got, added)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0] != added {
		t.Fatalf("List got %+v; want [%+v]", list, added)
	}

	updated, err := repo.Update(ctx, added.ID, "/music/jazz", "Jazz", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.ID != added.ID || updated.Path != "/music/jazz" || updated.Name != "Jazz" {
		t.Fatalf("Update returned %+v", updated)
	}

	if err := repo.Remove(ctx, added.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	_, err = repo.Get(ctx, added.ID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Get after Remove got %v; want sql.ErrNoRows", err)
	}
}

func TestAdd_RejectsDuplicatePath(t *testing.T) {
	ctx := context.Background()
	repo := library.New(openTestDB(t))

	if _, err := repo.Add(ctx, "/music", "Music", models.LibrarySettings{}); err != nil {
		t.Fatalf("Add initial: %v", err)
	}
	if _, err := repo.Add(ctx, "/music", "Duplicate", models.LibrarySettings{}); err == nil {
		t.Fatal("Add duplicate returned nil error; want constraint error")
	}
}

func TestAdd_ValidatesRequiredFields(t *testing.T) {
	ctx := context.Background()
	repo := library.New(openTestDB(t))

	tests := []struct {
		name string
		path string
		lib  string
	}{
		{name: "empty path", path: "", lib: "Music"},
		{name: "empty name", path: "/music", lib: ""},
		{name: "blank path", path: "  ", lib: "Music"},
		{name: "blank name", path: "/music", lib: "  "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := repo.Add(ctx, tc.path, tc.lib, models.LibrarySettings{}); err == nil {
				t.Fatal("Add returned nil error; want validation error")
			}
		})
	}
}

func TestGetByName(t *testing.T) {
	ctx := context.Background()
	repo := library.New(openTestDB(t))

	added, err := repo.Add(ctx, "/music/rock", "Rock", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := repo.GetByName(ctx, "Rock")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got != added {
		t.Fatalf("GetByName = %+v; want %+v", got, added)
	}

	if _, err := repo.GetByName(ctx, "  Rock  "); err != nil {
		t.Fatalf("GetByName trimmed: %v", err)
	}

	if _, err := repo.GetByName(ctx, ""); err == nil {
		t.Fatal("GetByName empty name returned nil error")
	}
	if _, err := repo.GetByName(ctx, "    "); err == nil {
		t.Fatal("GetByName whitespace name returned nil error")
	}

	if _, err := repo.GetByName(ctx, "Missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetByName missing = %v; want sql.ErrNoRows", err)
	}
}

func TestUpdateRemove_NotFound(t *testing.T) {
	ctx := context.Background()
	repo := library.New(openTestDB(t))

	_, err := repo.Update(ctx, 123, "/music", "Music", models.LibrarySettings{})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Update missing got %v; want sql.ErrNoRows", err)
	}
	err = repo.Remove(ctx, 123)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Remove missing got %v; want sql.ErrNoRows", err)
	}
}

func TestGetByName_AmbiguousReturnsError(t *testing.T) {
	ctx := context.Background()
	repo := library.New(openTestDB(t))

	if _, err := repo.Add(ctx, "/music/a", "Music", models.LibrarySettings{}); err != nil {
		t.Fatalf("Add a: %v", err)
	}
	if _, err := repo.Add(ctx, "/music/b", "Music", models.LibrarySettings{}); err != nil {
		t.Fatalf("Add b: %v", err)
	}

	_, err := repo.GetByName(ctx, "Music")
	if err == nil {
		t.Fatal("GetByName ambiguous returned nil error")
	}
	if !errors.Is(err, library.ErrAmbiguousLibraryName) {
		t.Fatalf("err = %v; want errors.Is(_, ErrAmbiguousLibraryName)", err)
	}
}
