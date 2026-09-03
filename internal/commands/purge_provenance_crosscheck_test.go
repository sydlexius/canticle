package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
)

// setPurgeLane stamps work_queue.provider_lane on the seeded row for filename.
func setPurgeLane(t *testing.T, ctx context.Context, dbPath, filename, lane string) {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // reason: test cleanup; a close failure cannot affect the assertions
	res, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET provider_lane = ? WHERE title = ?`, lane, filename)
	if err != nil {
		t.Fatalf("set provider_lane: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if n != 1 {
		t.Fatalf("set provider_lane matched %d rows; want 1 -- the fixture did not seed what this test assumes", n)
	}
}

// TestPurgeProvenance_DisputedProvenanceIsRefusedAndReported is the CLI half of
// the #827 deletion hazard: a sidecar whose [source:] tag names a different
// provider than work_queue.provider_lane is not deleted, the summary says so,
// and the dry-run preview agrees with what apply actually does.
func TestPurgeProvenance_DisputedProvenanceIsRefusedAndReported(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupPurgeProvenance(t)
	target := filepath.Join(root, "ArtistA", "one.lrc")
	writePurgeSidecar(t, target, "musixmatch")
	seedPurgeTrack(t, ctx, dbPath, filepath.Dir(target), "one.lrc", "done")
	setPurgeLane(t, ctx, dbPath, "one.lrc", "petitlyrics")

	var dry bytes.Buffer
	if code := runPurgeProvenance(ctx, &dry, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch"}); code != 0 {
		t.Fatalf("dry exit=%d out=%s", code, dry.String())
	}
	if !strings.Contains(dry.String(), "would delete 0") {
		t.Errorf("dry run must not promise a deletion it will refuse; got: %s", dry.String())
	}
	if !strings.Contains(dry.String(), "disagrees with the provider recorded in the database") {
		t.Errorf("dry run must explain the refusal; got: %s", dry.String())
	}

	var apply bytes.Buffer
	if code := runPurgeProvenance(ctx, &apply, ScanPurgeProvenanceCmd{ConfigPath: cfgPath, Source: "musixmatch", Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, apply.String())
	}
	if !strings.Contains(apply.String(), "deleted 0") {
		t.Errorf("apply must delete nothing; got: %s", apply.String())
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("the disputed sidecar was deleted: %v", statErr)
	}

	// Privacy: a work_queue row carries the library's private metadata, so the
	// refusal notice must name neither the path nor the artist/title.
	for _, secret := range []string{target, "ArtistA", "one.lrc", "Title"} {
		if strings.Contains(apply.String(), secret) {
			t.Errorf("refusal output leaked private library metadata %q; got: %s", secret, apply.String())
		}
	}
}
