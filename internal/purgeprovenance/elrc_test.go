package purgeprovenance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/sidecar"
)

// A purged .lrc takes its OWNED word-synced companion (#986) with it, backup
// first; a foreign companion is left exactly as it was. The companion is never
// judged on its own tags.
func TestRun_CompanionFollowsItsPurgedLrc(t *testing.T) {
	const owned = "[by:canticle]\n[source:musixmatch]\n[00:01.00]<00:01.00>hi\n"
	cases := []struct {
		name         string
		active       bool
		file         string
		dryRun       bool
		elrc         string
		wantDeleted  bool
		wantReported int
	}{
		{"owned", true, "track.lrc", false, owned, true, 2},
		{"owned dry run", true, "track.lrc", true, owned, false, 2},
		{"foreign", true, "track.lrc", false, "[source:musixmatch]\n[00:01.00]<00:01.00>hi\n", false, 1},
		// Only a .lrc has a companion: a purged .txt leaves track.elrc alone.
		{"purged txt", true, "track.txt", false, owned, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.active {
				sidecar.ActivateForTest(t, sidecar.KindWordSynced)
			}
			ctx, sqlDB, libID, root := openSeeded(t)
			lrc := filepath.Join(root, "Album", tc.file)
			writeSidecar(t, lrc, "musixmatch")
			seedTrack(t, ctx, sqlDB, libID, filepath.Dir(lrc), tc.file, "done")
			elrc := filepath.Join(root, "Album", "track.elrc")
			if err := os.WriteFile(elrc, []byte(tc.elrc), 0o600); err != nil {
				t.Fatal(err)
			}
			var reported []string
			res, err := New(sqlDB).Run(ctx, Options{
				Roots:  []string{root},
				Filter: Filter{Source: "musixmatch"},
				DryRun: tc.dryRun,
				Report: func(r Record) error { reported = append(reported, r.Path); return nil },
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if want := map[bool]int{true: 0, false: 1}[tc.dryRun]; res.Scanned != 1 || res.Deleted != want {
				t.Fatalf("scanned=%d deleted=%d; want 1/%d (the companion is not a sidecar of its own)", res.Scanned, res.Deleted, want)
			}
			if _, err := os.Stat(lrc); os.IsNotExist(err) == tc.dryRun {
				t.Fatalf("sidecar gone=%v on dryRun=%v", os.IsNotExist(err), tc.dryRun)
			}
			_, serr := os.Stat(elrc)
			if gone := os.IsNotExist(serr); gone != tc.wantDeleted {
				t.Fatalf("companion deleted=%v; want %v", gone, tc.wantDeleted)
			}
			if len(reported) != tc.wantReported || (tc.wantReported == 2 && reported[1] != elrc) {
				t.Errorf("reported %v; want %d record(s), the companion after its .lrc", reported, tc.wantReported)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.wantDeleted]; res.CompanionsDeleted != want {
				t.Errorf("CompanionsDeleted = %d; want %d", res.CompanionsDeleted, want)
			}
		})
	}
}

// A failed companion step (its backup record, or its delete, which runs BEFORE
// the .lrc's) leaves the .lrc too, so the pair is never split.
func TestRun_FailedCompanionStepLeavesThePair(t *testing.T) {
	for _, failDelete := range []bool{false, true} {
		t.Run(map[bool]string{false: "backup", true: "delete"}[failDelete], func(t *testing.T) {
			sidecar.ActivateForTest(t, sidecar.KindWordSynced)
			ctx, sqlDB, libID, root := openSeeded(t)
			lrc := filepath.Join(root, "Album", "track.lrc")
			writeSidecar(t, lrc, "musixmatch")
			seedTrack(t, ctx, sqlDB, libID, filepath.Dir(lrc), "track.lrc", "done")
			if err := os.WriteFile(strings.TrimSuffix(lrc, ".lrc")+".elrc", []byte("[by:canticle]\n[00:01.00]<00:01.00>hi\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			prev := removeFile
			removeFile = func(p string) error {
				if failDelete && strings.HasSuffix(p, ".elrc") {
					return os.ErrPermission
				}
				return prev(p)
			}
			t.Cleanup(func() { removeFile = prev })
			res, err := New(sqlDB).Run(ctx, Options{Roots: []string{root}, Filter: Filter{Source: "musixmatch"}, Report: func(r Record) error {
				if !failDelete && strings.HasSuffix(r.Path, ".elrc") {
					return errors.New("disk full")
				}
				return nil
			}})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if _, serr := os.Stat(lrc); serr != nil || res.Errors != 1 || res.Deleted != 0 {
				t.Errorf("lrc stat=%v errors=%d deleted=%d; want the .lrc kept after a failed companion step", serr, res.Errors, res.Deleted)
			}
		})
	}
}
