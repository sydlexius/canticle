package library_test

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

func bp(v bool) *bool { return &v }

func wantBoolPtr(t *testing.T, label string, got *bool, want *bool) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("%s = %v; want nil", label, *got)
	case want != nil && got == nil:
		t.Fatalf("%s = nil; want %v", label, *want)
	case want != nil && got != nil && *got != *want:
		t.Fatalf("%s = %v; want %v", label, *got, *want)
	}
}

func TestAddPersistsSettings(t *testing.T) {
	ctx := context.Background()
	repo := library.New(openTestDB(t))

	// Unset settings round-trip as NULL -> nil (inherit global default).
	inherit, err := repo.Add(ctx, "/music/inherit", "Inherit", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add inherit: %v", err)
	}
	wantBoolPtr(t, "inherit.EnrichRecording", inherit.EnrichRecording, nil)
	wantBoolPtr(t, "inherit.DetectInstrumental", inherit.DetectInstrumental, nil)

	// Explicit values round-trip as 0/1 -> *bool.
	explicit, err := repo.Add(ctx, "/music/explicit", "Explicit", models.LibrarySettings{
		EnrichRecording:    bp(true),
		DetectInstrumental: bp(false),
	})
	if err != nil {
		t.Fatalf("Add explicit: %v", err)
	}
	wantBoolPtr(t, "explicit.EnrichRecording", explicit.EnrichRecording, bp(true))
	wantBoolPtr(t, "explicit.DetectInstrumental", explicit.DetectInstrumental, bp(false))

	// Values survive a fresh Get.
	got, err := repo.Get(ctx, explicit.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	wantBoolPtr(t, "get.EnrichRecording", got.EnrichRecording, bp(true))
	wantBoolPtr(t, "get.DetectInstrumental", got.DetectInstrumental, bp(false))

	// List must surface the same settings (exercises the List SELECT/Scan path,
	// which is distinct from Get's single-row query).
	libs, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var listed *models.Library
	for i := range libs {
		if libs[i].ID == explicit.ID {
			listed = &libs[i]
		}
	}
	if listed == nil {
		t.Fatalf("List did not return library %d", explicit.ID)
	}
	wantBoolPtr(t, "list.EnrichRecording", listed.EnrichRecording, bp(true))
	wantBoolPtr(t, "list.DetectInstrumental", listed.DetectInstrumental, bp(false))
}

func TestUpdatePreservesAndSetsSettings(t *testing.T) {
	ctx := context.Background()
	repo := library.New(openTestDB(t))

	base, err := repo.Add(ctx, "/music/base", "Base", models.LibrarySettings{
		EnrichRecording:    bp(true),
		DetectInstrumental: bp(true),
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Nil settings leave both columns unchanged.
	preserved, err := repo.Update(ctx, base.ID, "/music/base2", "Base2", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Update preserve: %v", err)
	}
	wantBoolPtr(t, "preserved.EnrichRecording", preserved.EnrichRecording, bp(true))
	wantBoolPtr(t, "preserved.DetectInstrumental", preserved.DetectInstrumental, bp(true))

	// A non-nil field is written; the absent field stays unchanged.
	updated, err := repo.Update(ctx, base.ID, "/music/base2", "Base2", models.LibrarySettings{
		EnrichRecording: bp(false),
	})
	if err != nil {
		t.Fatalf("Update set: %v", err)
	}
	wantBoolPtr(t, "updated.EnrichRecording", updated.EnrichRecording, bp(false))
	wantBoolPtr(t, "updated.DetectInstrumental", updated.DetectInstrumental, bp(true))
}
