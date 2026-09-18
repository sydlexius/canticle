package realign

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The word-synced companion (.elrc, #986) follows its .lrc through every
// mutation here.

const (
	ownedElrc   = "[by:canticle]\n[00:01.00]<00:01.00>hi\n"
	foreignElrc = "[00:01.00]<00:01.00>someone else's\n"
)

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// planRename: one orphan .lrc (tagged with an ISRC) and a companion beside it,
// plus the renamed audio. apply returns the outcomes and the backup content.
func planRename(t *testing.T, elrcBody, occupiedNewElrc string) (orphanElrc, newLrc, newElrc string, res Result, apply func() ([]Applied, string)) {
	t.Helper()
	root := tempRoot(t)
	dir := filepath.Join(root, "Album")
	audio := filepath.Join(dir, "new.flac")
	write(t, audio, "a")
	write(t, filepath.Join(dir, "old.lrc"), "[isrc:US1]\n[00:01.00]hi\n")
	orphanElrc = filepath.Join(dir, "old.elrc")
	write(t, orphanElrc, elrcBody)
	if occupiedNewElrc != "" {
		write(t, filepath.Join(dir, "new.elrc"), occupiedNewElrc)
	}
	r, lib := newRealigner(root, defaultCfg(), map[string]string{audio: "US1"})
	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	backup := filepath.Join(t.TempDir(), "b.jsonl")
	return orphanElrc, filepath.Join(dir, "new.lrc"), filepath.Join(dir, "new.elrc"), res, func() ([]Applied, string) {
		applied, aerr := r.Apply(res.Moves, backup, Policy{AllowHeuristic: true})
		if aerr != nil {
			t.Fatalf("Apply: %v", aerr)
		}
		b, _ := os.ReadFile(backup)
		return applied, string(b)
	}
}

// Only an OWNED companion follows its renamed .lrc; it is
// never planned as an orphan of its own. One whose destination is occupied
// blocks the whole move at plan time rather than splitting the pair.
func TestRename_CompanionFollowsOnlyWhenOwned(t *testing.T) {
	for _, c := range []struct {
		name           string
		body, occupied string
		wantFollowed   bool
	}{{"owned", ownedElrc, "", true}, {"foreign", foreignElrc, "", false}, {"blocked", ownedElrc, foreignElrc, false}} {
		t.Run(c.name, func(t *testing.T) {
			oldElrc, newLrc, newElrc, res, apply := planRename(t, c.body, c.occupied)
			if c.occupied != "" {
				if len(res.Moves) != 0 || len(res.Skips) != 1 || res.Skips[0].Kind != "conflict" {
					t.Fatalf("moves=%+v skips=%+v; want one conflict skip and no move", res.Moves, res.Skips)
				}
				return
			}
			if len(res.Moves) != 1 || strings.HasSuffix(res.Moves[0].Orphan, ".elrc") {
				t.Fatalf("moves = %+v; want exactly the .lrc planned", res.Moves)
			}
			if a, _ := apply(); a[0].Err != nil || !exists(newLrc) {
				t.Fatalf("apply: %v; lrc moved=%v", a[0].Err, exists(newLrc))
			}
			if exists(newElrc) != c.wantFollowed || exists(oldElrc) == c.wantFollowed {
				t.Errorf("companion new=%v old=%v; want followed=%v", exists(newElrc), exists(oldElrc), c.wantFollowed)
			}
		})
	}
}

// A lone .elrc (foreign, or stranded by a crash) is never coverage: counting it
// turns an ambiguous directory into a wrong-winner auto-move, or hides the gap
// a correct re-attachment needs.
func TestClassify_LoneCompanionIsNotCoverage(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		wantMoves int
	}{
		{"foreign masks a gap", map[string]string{"01 Alpha.flac": "a", "01 Alpha.elrc": foreignElrc, "02 Beta.flac": "b", "lyrics.txt": "instrumental\n"}, 0},
		{"stranded masks a gap", map[string]string{"01 Alpha.flac": "a", "01 Alpha.elrc": ownedElrc, "02 Beta.flac": "b", "lyrics.txt": "instrumental\n"}, 0},
		{"foreign hides the only gap", map[string]string{"01 Song.flac": "a", "01 Song.elrc": foreignElrc, "Song.lrc": "[00:01.00]hi\n"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := tempRoot(t)
			for name, body := range c.files {
				write(t, filepath.Join(root, "Album", name), body)
			}
			r, lib := newRealigner(root, defaultCfg(), nil)
			res, err := r.PlanLibrary(lib)
			if err != nil {
				t.Fatalf("PlanLibrary: %v", err)
			}
			if len(res.Moves) != c.wantMoves {
				t.Fatalf("moves=%+v skips=%+v; want %d move(s)", res.Moves, res.Skips, c.wantMoves)
			}
		})
	}
}

// A rename whose companion step fails AFTER the .lrc moved moves the .lrc back
// -- unless the old path was re-occupied meanwhile: then the undo must not
// clobber it, and the backup line stays since the .lrc move stands.
func TestRename_FailedCompanionStepUndoesTheLrcMove(t *testing.T) {
	for _, reoccupy := range []bool{false, true} {
		t.Run(map[bool]string{false: "undo", true: "reoccupied"}[reoccupy], func(t *testing.T) {
			oldElrc, newLrc, newElrc, _, apply := planRename(t, ownedElrc, "")
			oldLrc := strings.TrimSuffix(oldElrc, ".elrc") + ".lrc"
			prev := renameFile
			renameFile = func(oldpath, newpath string) error {
				if strings.HasSuffix(oldpath, ".elrc") {
					if reoccupy {
						write(t, oldLrc, "FRESH")
					}
					return os.ErrPermission
				}
				return prev(oldpath, newpath)
			}
			t.Cleanup(func() { renameFile = prev })
			a, backup := apply()
			if a[0].Err == nil {
				t.Fatalf("a failed companion move reported success")
			}
			if !exists(oldElrc) || exists(newElrc) {
				t.Errorf("companion moved: old=%v new=%v", exists(oldElrc), exists(newElrc))
			}
			if !reoccupy && (!exists(oldLrc) || exists(newLrc)) {
				t.Errorf("pair split: oldLrc=%v newLrc=%v; want the .lrc back on the old stem", exists(oldLrc), exists(newLrc))
			}
			if b, _ := os.ReadFile(oldLrc); reoccupy && (string(b) != "FRESH" || !exists(newLrc) || strings.TrimSpace(backup) == "") {
				t.Errorf("undo clobbered a re-occupied old path: old=%q newLrc=%v backup=%q", b, exists(newLrc), backup)
			}
		})
	}
}

// remediate applies one remediation of kind to a .lrc with a companion beside
// it; mut, if non-nil, adjusts the fixture and Move first.
func remediate(t *testing.T, kind, elrcBody string, mut func(*testing.T, *Move)) (string, string, string, Applied, string) {
	t.Helper()
	root := t.TempDir()
	lrc := filepath.Join(root, "track.lrc")
	write(t, lrc, fixtureBody)
	elrc := filepath.Join(root, "track.elrc")
	write(t, elrc, elrcBody)
	mv := Move{Orphan: lrc, Target: filepath.Join(t.TempDir(), "q", "track.lrc"), Kind: kind, Eligible: true}
	if kind == KindDemote {
		mv.TextPath = filepath.Join(root, "track.txt")
		mv.TextBody = "alpha\n"
	}
	if mut != nil {
		mut(t, &mv)
	}
	got, backup := applyOne(t, mv)
	return lrc, elrc, mv.Target, got, backup
}

// An owned companion goes with its remediated .lrc and is recorded in the
// backup; a foreign one is untouched.
func TestRemediation_CompanionGoesWithTheLrcOnlyWhenOwned(t *testing.T) {
	for _, kind := range []string{KindDemote, KindQuarantine, KindPurge} {
		for _, c := range []struct {
			name         string
			body         string
			wantFollowed bool
		}{{"owned", ownedElrc, true}, {"foreign", foreignElrc, false}} {
			t.Run(kind+"/"+c.name, func(t *testing.T) {
				lrc, elrc, target, got, backup := remediate(t, kind, c.body, nil)
				if got.Err != nil || exists(lrc) {
					t.Fatalf("%s: %v; lrc still present=%v", kind, got.Err, exists(lrc))
				}
				if b, err := os.ReadFile(elrc); c.wantFollowed == (err == nil) || (!c.wantFollowed && string(b) != c.body) {
					t.Fatalf("companion beside the .lrc: %q, %v; want gone=%v", b, err, c.wantFollowed)
				}
				for _, name := range dirNames(t, filepath.Dir(lrc)) {
					if strings.Contains(name, ".purge-") {
						t.Errorf("staged companion left behind after a successful %s: %s", kind, name)
					}
				}
				var rec backupRecord
				if err := json.Unmarshal([]byte(strings.TrimSpace(backup)), &rec); err != nil {
					t.Fatalf("backup: %v (%s)", err, backup)
				}
				wantPath, wantNew := "", ""
				if c.wantFollowed {
					wantPath = elrc
					if kind != KindPurge {
						wantNew = companionTarget(target)
					}
				}
				if rec.CompanionPath != wantPath || rec.CompanionNewPath != wantNew || (wantNew != "" && !exists(wantNew)) {
					t.Errorf("backup companion %q -> %q (exists=%v); want %q -> %q", rec.CompanionPath, rec.CompanionNewPath, exists(wantNew), wantPath, wantNew)
				}
			})
		}
	}
}

// dirNames lists dir's entries, so a test can see a stray staged companion.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// A refused remediation leaves the .lrc, never overwrites an occupied
// destination, and keeps the companion with its backup line dropped. That
// includes a purge failing AFTER its companion was staged: the stage is put
// back, so the pair is exactly as it was and nothing else is left beside it.
func TestRemediation_RefusalsKeepThePairOrItsRecord(t *testing.T) {
	cases := []struct {
		name string
		kind string
		mut  func(t *testing.T, mv *Move)
	}{
		{"occupied lrc target", KindQuarantine, func(t *testing.T, mv *Move) { write(t, mv.Target, "EARLIER") }},
		{"occupied companion target", KindQuarantine, func(t *testing.T, mv *Move) { write(t, companionTarget(mv.Target), "EARLIER") }},
		{"symlinked lrc", KindPurge, func(t *testing.T, mv *Move) {
			real := mv.Orphan + ".real"
			if err := os.Rename(mv.Orphan, real); err != nil || os.Symlink(real, mv.Orphan) != nil {
				t.Fatalf("symlink fixture: %v", err)
			}
		}},
		{"demote without text path", KindDemote, func(_ *testing.T, mv *Move) { mv.TextPath = "" }},
		{"quarantine without target", KindQuarantine, func(_ *testing.T, mv *Move) { mv.Target = "" }},
		{"purge whose text write fails", KindPurge, func(_ *testing.T, mv *Move) {
			mv.TextPath, mv.TextBody = filepath.Join(filepath.Dir(mv.Orphan), "nodir", "x.txt"), "alpha\n"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lrc, elrc, target, got, backup := remediate(t, c.kind, ownedElrc, c.mut)
			if got.Err == nil || !exists(lrc) {
				t.Fatalf("%s: err=%v lrc kept=%v; want a refusal with the .lrc in place", c.name, got.Err, exists(lrc))
			}
			if b, err := os.ReadFile(elrc); err != nil || string(b) != ownedElrc {
				t.Errorf("companion: %q, %v; want it untouched", b, err)
			}
			for _, name := range dirNames(t, filepath.Dir(lrc)) {
				if strings.Contains(name, ".purge-") {
					t.Errorf("staged companion left behind: %s", name)
				}
			}
			for _, p := range []string{target, companionTarget(target)} {
				if b, err := os.ReadFile(p); target != "" && err == nil && string(b) != "EARLIER" {
					t.Errorf("destination %s written by a refused action: %q", filepath.Base(p), b)
				}
			}
			if strings.TrimSpace(backup) != "" {
				t.Errorf("backup line kept for a refusal that left the pair untouched: %q", backup)
			}
		})
	}
}

// A purge whose .lrc step fails AND whose staged companion cannot be put back
// has mutated the library: the backup line naming the companion must stay.
func TestRemediation_PurgeRestoreFailureKeepsTheRecord(t *testing.T) {
	prev := renameFile
	renameFile = func(oldpath, newpath string) error {
		if strings.Contains(filepath.Base(oldpath), ".purge-") {
			return os.ErrPermission
		}
		return prev(oldpath, newpath)
	}
	t.Cleanup(func() { renameFile = prev })
	lrc, elrc, _, got, backup := remediate(t, KindPurge, ownedElrc, func(_ *testing.T, mv *Move) {
		mv.TextPath, mv.TextBody = filepath.Join(filepath.Dir(mv.Orphan), "nodir", "x.txt"), "alpha\n"
	})
	if got.Err == nil || !exists(lrc) || exists(elrc) {
		t.Fatalf("err=%v lrc=%v elrc=%v; want a failure with the .lrc kept and the companion still staged", got.Err, exists(lrc), exists(elrc))
	}
	if !strings.Contains(backup, `"companion_path"`) {
		t.Errorf("backup line dropped although the companion could not be restored: %q", backup)
	}
}

// A cross-device companion move whose copy landed but whose source could not
// be removed leaves both copies: that is a mutation, so the backup line stays.
// On the remediation path the companion goes first, so the .lrc is not touched;
// on the rename path the .lrc already moved and stays with its companion
// rather than being undone away from it.
func TestCompanion_PartialCrossDeviceMoveKeepsTheRecord(t *testing.T) {
	for _, path := range []string{"quarantine", "rename"} {
		t.Run(path, func(t *testing.T) {
			prevRename, prevRemove := renameFile, removeSource
			renameFile = func(oldpath, newpath string) error {
				if strings.HasSuffix(oldpath, ".elrc") {
					return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
				}
				return prevRename(oldpath, newpath)
			}
			removeSource = func(string) error { return os.ErrPermission }
			t.Cleanup(func() { renameFile, removeSource = prevRename, prevRemove })

			var got Applied
			var backup, newLrc, newElrc string
			if path == "quarantine" {
				var lrc, target string
				lrc, _, target, got, backup = remediate(t, KindQuarantine, ownedElrc, nil)
				if exists(target) {
					t.Errorf("the .lrc was quarantined after its companion step failed")
				}
				newLrc, newElrc = lrc, companionTarget(target)
			} else {
				var apply func() ([]Applied, string)
				var a []Applied
				_, newLrc, newElrc, _, apply = planRename(t, ownedElrc, "")
				a, backup = apply()
				got = a[0]
			}
			if got.Err == nil {
				t.Fatalf("a failed source unlink reported success")
			}
			if !exists(newElrc) || !exists(newLrc) {
				t.Errorf("lrc=%v companion copy=%v; want both present", exists(newLrc), exists(newElrc))
			}
			if !strings.Contains(backup, `"companion_new_path"`) {
				t.Errorf("backup line dropped although the companion copy landed: %q", backup)
			}
		})
	}
}

// planBare: an orphan .lrc WITHOUT a companion (ISRC-tagged) and its renamed
// audio. A non-empty stranded body is written at the target's companion path
// before planning when atPlan, else after it, so only Apply's re-check can see
// it. #997.
func planBare(t *testing.T, stranded string, atPlan bool) (newLrc, newElrc string, res Result, apply func() []Applied) {
	t.Helper()
	root := tempRoot(t)
	dir := filepath.Join(root, "Album")
	audio := filepath.Join(dir, "new.flac")
	write(t, audio, "a")
	write(t, filepath.Join(dir, "old.lrc"), "[isrc:US1]\n[00:01.00]hi\n")
	newLrc, newElrc = filepath.Join(dir, "new.lrc"), filepath.Join(dir, "new.elrc")
	if stranded != "" && atPlan {
		write(t, newElrc, stranded)
	}
	r, lib := newRealigner(root, defaultCfg(), map[string]string{audio: "US1"})
	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	if stranded != "" && !atPlan {
		write(t, newElrc, stranded)
	}
	return newLrc, newElrc, res, func() []Applied {
		applied, aerr := r.Apply(res.Moves, filepath.Join(t.TempDir(), "b.jsonl"), Policy{AllowHeuristic: true})
		if aerr != nil {
			t.Fatalf("Apply: %v", aerr)
		}
		return applied
	}
}

// An orphan .lrc with no companion never lands beside a stale OWNED .elrc: the
// move is refused (a plan-time conflict, or Apply's re-check when the companion
// appeared after planning), and the stale companion is left for the next write
// of that stem, which owns its removal. #997.
func TestRename_BareLrcNeverLandsBesideAStaleOwnedCompanion(t *testing.T) {
	t.Run("plan", func(t *testing.T) {
		newLrc, newElrc, res, _ := planBare(t, ownedElrc, true)
		if len(res.Moves) != 0 || len(res.Skips) != 1 || res.Skips[0].Kind != "conflict" ||
			!strings.Contains(res.Skips[0].Reason, "word-synced companion") {
			t.Fatalf("moves=%+v skips=%+v; want one companion conflict skip and no move", res.Moves, res.Skips)
		}
		if exists(newLrc) || !exists(newElrc) {
			t.Errorf("lrc=%v stale companion=%v; want no lrc and the companion untouched", exists(newLrc), exists(newElrc))
		}
	})
	t.Run("apply", func(t *testing.T) {
		newLrc, newElrc, res, apply := planBare(t, ownedElrc, false)
		if len(res.Moves) != 1 {
			t.Fatalf("moves=%+v skips=%+v; want the move planned", res.Moves, res.Skips)
		}
		if a := apply(); a[0].Err == nil || exists(newLrc) {
			t.Fatalf("apply err=%v lrc landed=%v; want a refusal and no lrc", a[0].Err, exists(newLrc))
		}
		if b, err := os.ReadFile(newElrc); err != nil || string(b) != ownedElrc {
			t.Errorf("stale companion = %q, %v; want it untouched", b, err)
		}
	})
}

// A FOREIGN .elrc at the target blocks nothing (the writer never touches one
// either) and is byte-identical afterwards. #997.
func TestRename_BareLrcBesideForeignCompanionMovesAndLeavesItAlone(t *testing.T) {
	newLrc, newElrc, res, apply := planBare(t, foreignElrc, true)
	if len(res.Moves) != 1 || len(res.Skips) != 0 {
		t.Fatalf("moves=%+v skips=%+v; want the move planned", res.Moves, res.Skips)
	}
	if a := apply(); a[0].Err != nil || !exists(newLrc) {
		t.Fatalf("apply err=%v lrc landed=%v; want the move applied", a[0].Err, exists(newLrc))
	}
	if b, err := os.ReadFile(newElrc); err != nil || string(b) != foreignElrc {
		t.Errorf("foreign companion = %q, %v; want it byte-identical", b, err)
	}
}
