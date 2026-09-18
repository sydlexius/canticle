package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// The provenance backfill is .lrc-only: an OWNED word-synced companion (#986)
// beside the .lrc is left byte-for-byte untouched, and its presence does not
// change the summary. No canticle path reads a companion's tags
// (purge-provenance removes an owned companion by deriving it from its matching
// .lrc), so rewriting it would modify a user file for no reader.
func TestRunProvenanceBackfill_LeavesWordSyncedCompanionUntouched(t *testing.T) {
	const owned = "[by:canticle]\n[ar:Test Artist]\n[ti:Test Track]\n[00:01.00]<00:01.00>Hello <00:01.50>world\n"

	run := func(t *testing.T, withCompanion bool) (summary, companion string) {
		t.Helper()
		ctx := context.Background()
		tmpDB := filepath.Join(t.TempDir(), "test.db")
		sqlDB, err := db.Open(ctx, tmpDB)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		outdir := t.TempDir()
		lrcPath := filepath.Join(outdir, testLRCFilename)
		writeLRCFile(t, lrcPath)
		companion = sidecar.StemOf(lrcPath) + sidecar.ExtWordSynced
		if withCompanion {
			if err := os.WriteFile(companion, []byte(owned), 0o600); err != nil {
				t.Fatalf("write companion: %v", err)
			}
		}
		song := &models.Song{Track: models.Track{ArtistName: "Test Artist", TrackName: "Test Track"}}
		seedProvenanceDB(t, sqlDB, outdir, "musixmatch", time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC), song)
		_ = sqlDB.Close()

		var out bytes.Buffer
		args := ProvenanceBackfillCmd{ConfigPath: makeTempConfig(t, tmpDB), Paths: []string{outdir}, Yes: true}
		if code := runProvenanceBackfill(ctx, &out, args); code != 0 {
			t.Fatalf("exit=%d, output=%s", code, out.String())
		}
		lrc, err := os.ReadFile(lrcPath)
		if err != nil {
			t.Fatalf("read .lrc: %v", err)
		}
		if !strings.Contains(string(lrc), "[source:musixmatch]") {
			t.Fatalf(".lrc was not tagged: %q", lrc)
		}
		return out.String(), companion
	}

	baseline, _ := run(t, false)
	if !strings.Contains(baseline, "applied 1,") {
		t.Fatalf("baseline summary = %q; want one applied .lrc", baseline)
	}
	withCompanion, companion := run(t, true)
	if withCompanion != baseline {
		t.Fatalf("summary changed by the companion's presence\n got: %q\nwant: %q", withCompanion, baseline)
	}
	got, err := os.ReadFile(companion)
	if err != nil {
		t.Fatalf("read companion: %v", err)
	}
	if string(got) != owned {
		t.Fatalf("owned companion was modified; want byte-identical\n got: %q\nwant: %q", got, owned)
	}
}
