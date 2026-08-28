package revalidate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/realign"
)

// These are APPLY-level tests. Every other test in this package stops at Plan or
// Validate, which is structurally incapable of seeing a move that is planned
// coherently and then fails when realign tries to perform it -- exactly the
// class of defect this file exists to catch.

// TestPurgeWithDemoteActuallyRemediates is the regression for the case
// `canticle revalidate --purge --apply` hits on every MisSynced file.
//
// --purge with the DEFAULT --on-fail demote planned a move with Kind=demote and
// an EMPTY Target: removalMove returned KindPurge/no-Target because Purge was
// set, and the demote path then relabelled the Kind while leaving Target empty.
// realign's demote arm writes the .txt and then calls moveAside, which refuses
// an empty target -- so the .txt was rolled back and the .lrc left in place.
// The run reported a failure per file and remediated nothing.
//
// The words are content-correct, so the honest meaning of demote+purge is
// "keep the words as .txt, then delete the .lrc" -- not "fail".
func TestPurgeWithDemoteActuallyRemediates(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	audio := filepath.Join(filepath.Dir(lrc), "track.mp3")
	txt := filepath.Join(filepath.Dir(lrc), "track.txt")

	r, _ := newRevalidator(t, root, fixedDuration(), func(o *Options) {
		o.Purge = true // --purge, with --on-fail left at its demote default
	})
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Moves) != 1 {
		t.Fatalf("len(Moves) = %d; want 1", len(plan.Moves))
	}

	for _, a := range applyPlan(t, plan) {
		if a.GatedSkipped {
			t.Fatal("move was gated-skipped; it should have applied")
		}
	}

	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Error("the .lrc survived a --purge run; it should have been deleted")
	}
	body, rerr := os.ReadFile(txt)
	if rerr != nil {
		t.Fatalf("the demoted .txt was not written: %v", rerr)
	}
	if len(body) == 0 {
		t.Error("the demoted .txt is empty; the words are the whole reason demote exists")
	}
	if _, err := os.Stat(audio); err != nil {
		t.Errorf("the audio file was disturbed: %v", err)
	}
}

// TestPurgeWithOnFailDeleteStillUnlinks pins the other legacy combination:
// --on-fail delete --purge keeps no words and unlinks.
func TestPurgeWithOnFailDeleteStillUnlinks(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	txt := filepath.Join(filepath.Dir(lrc), "track.txt")

	r, _ := newRevalidator(t, root, fixedDuration(), func(o *Options) {
		o.OnFail = Delete
		o.Purge = true
	})
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	applyPlan(t, plan)
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Error("the .lrc survived --on-fail delete --purge")
	}
	if _, err := os.Stat(txt); !os.IsNotExist(err) {
		t.Error("a .txt was written for --on-fail delete; that mode keeps no words")
	}
}

// TestQuarantineIsNotUpgradedToPurgeByTheLegacyFlag is the I1 regression.
//
// removalMove read opts.Purge directly and converted UNCONDITIONALLY, so a
// caller that explicitly asked for the recoverable action got the irreversible
// one whenever the legacy flag happened to be set. A file-moving API must never
// silently upgrade "move it aside" to "delete it".
func TestQuarantineIsNotUpgradedToPurgeByTheLegacyFlag(t *testing.T) {
	root, lrc := lib(t, overrunBody)

	r, quarantine := newRevalidator(t, root, fixedDuration(), func(o *Options) {
		o.MisSyncedAction = ActionQuarantine
		o.CategoricalAction = ActionQuarantine
		o.Purge = true // legacy flag set alongside an explicit action
	})
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Moves) != 1 {
		t.Fatalf("len(Moves) = %d; want 1", len(plan.Moves))
	}
	mv := plan.Moves[0]
	if mv.Kind != realign.KindQuarantine {
		t.Errorf("Kind = %q; want %q -- an explicit quarantine must not become a delete", mv.Kind, realign.KindQuarantine)
	}
	if mv.Target == "" {
		t.Error("Target is empty; a quarantine must carry its destination")
	}

	applyPlan(t, plan)
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Error("the .lrc was not moved out of the library")
	}
	rel, _ := filepath.Rel(root, lrc)
	if _, err := os.Stat(filepath.Join(quarantine, rel)); err != nil {
		t.Errorf("the .lrc is not in quarantine; it may have been deleted: %v", err)
	}
}

// TestExplicitDemoteIsNotUnlinkedByTheLegacyFlag closes the same hole as
// TestQuarantineIsNotUpgradedToPurgeByTheLegacyFlag, on the arm where it was
// reintroduced by the demote fix.
//
// An EXPLICIT ActionDemote is a contract: write the words, move the .lrc aside.
// The legacy --purge flag must not silently turn that second half into an
// irreversible unlink -- a caller who names the action is not asking about
// flags. Only a demote DERIVED from those flags carries their purge semantics.
func TestExplicitDemoteIsNotUnlinkedByTheLegacyFlag(t *testing.T) {
	root, lrc := lib(t, overrunBody)

	r, quarantine := newRevalidator(t, root, fixedDuration(), func(o *Options) {
		o.MisSyncedAction = ActionDemote // explicit
		o.Purge = true                   // legacy flag also set
	})
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Moves) != 1 {
		t.Fatalf("len(Moves) = %d; want 1", len(plan.Moves))
	}
	mv := plan.Moves[0]
	if mv.Kind != realign.KindDemote {
		t.Errorf("Kind = %q; want %q -- an explicit demote must move the .lrc aside, not unlink it", mv.Kind, realign.KindDemote)
	}
	if mv.Target == "" {
		t.Error("Target is empty; an explicit demote must carry its quarantine destination")
	}

	applyPlan(t, plan)

	rel, _ := filepath.Rel(root, lrc)
	if _, err := os.Stat(filepath.Join(quarantine, rel)); err != nil {
		t.Errorf("the .lrc is not in quarantine; it may have been unlinked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(lrc), "track.txt")); err != nil {
		t.Errorf("the demoted .txt was not written: %v", err)
	}
}

// TestExplicitDemoteWithNoQuarantineDirIsRefused is the validation half: an
// explicit demote always needs a destination, so Purge must not excuse its
// absence.
func TestExplicitDemoteWithNoQuarantineDirIsRefused(t *testing.T) {
	err := Options{MisSyncedAction: ActionDemote, CategoricalAction: ActionPurge, Purge: true}.Validate()
	if err == nil {
		t.Error("an explicit demote with no QuarantineDir was accepted; it has nowhere to move the .lrc")
	}
}
