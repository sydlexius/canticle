package revalidate

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/realign"
)

// The per-arm actions are the SINGLE internal representation of "what happens
// to a rejected sidecar". The CLI's --on-fail/--purge flags translate into them
// in New, so there is one vocabulary rather than two mechanisms that can
// disagree, and the serve sweep's [timing_validation] config sets them
// directly. These pin that translation: a mistranslation would silently change
// what an existing CLI invocation does to a user's files.

// Fixture bodies, written against trackSeconds (120). Tolerance is 2s and
// CategoricalRatio is 1.5, so 2:30 overruns into MisSynced and 5:00 clears
// 3:00 into Categorical.
const (
	overrunBody     = "[00:10.00]alpha\n[02:30.00]beta\n"
	categoricalBody = "[00:10.00]alpha\n[05:00.00]beta\n"
)

// planOne runs a full Plan over a one-sidecar library and returns the result.
// It uses the real entry point rather than an internal seam, so these tests
// exercise the path the sweep and the CLI actually take.
func planOne(t *testing.T, body string, mutate func(*Options)) Plan {
	t.Helper()
	root, _ := lib(t, body)
	r, _ := newRevalidator(t, root, fixedDuration(), mutate)
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan
}

// TestNewTranslatesCLIFlagsToActions verifies each legacy flag combination lands
// on the equivalent per-arm actions.
func TestNewTranslatesCLIFlagsToActions(t *testing.T) {
	tests := []struct {
		name              string
		opts              Options
		wantOnOverrun     Action
		wantOnCategorical Action
	}{
		{
			name:              "defaults are demote and quarantine",
			opts:              Options{QuarantineDir: "/q"},
			wantOnOverrun:     ActionDemote,
			wantOnCategorical: ActionQuarantine,
		},
		{
			name:              "on-fail delete drops the words but still quarantines the file",
			opts:              Options{OnFail: Delete, QuarantineDir: "/q"},
			wantOnOverrun:     ActionQuarantine,
			wantOnCategorical: ActionQuarantine,
		},
		{
			name:              "purge makes removal irreversible on both arms",
			opts:              Options{Purge: true},
			wantOnOverrun:     ActionDemote,
			wantOnCategorical: ActionPurge,
		},
		{
			name:              "on-fail delete plus purge is the fully destructive combination",
			opts:              Options{OnFail: Delete, Purge: true},
			wantOnOverrun:     ActionPurge,
			wantOnCategorical: ActionPurge,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New(nil, tc.opts)
			if r.opts.MisSyncedAction != tc.wantOnOverrun {
				t.Errorf("MisSyncedAction = %q; want %q", r.opts.MisSyncedAction, tc.wantOnOverrun)
			}
			if r.opts.CategoricalAction != tc.wantOnCategorical {
				t.Errorf("CategoricalAction = %q; want %q", r.opts.CategoricalAction, tc.wantOnCategorical)
			}
		})
	}
}

// TestExplicitActionsWinOverFlags verifies a caller that sets the actions
// directly (the serve sweep, driven by [timing_validation] config) is not
// overwritten by the flag-derived defaults.
func TestExplicitActionsWinOverFlags(t *testing.T) {
	r := New(nil, Options{
		MisSyncedAction:   ActionOff,
		CategoricalAction: ActionPurge,
		QuarantineDir:     "/q",
	})
	if r.opts.MisSyncedAction != ActionOff {
		t.Errorf("MisSyncedAction = %q; want off", r.opts.MisSyncedAction)
	}
	if r.opts.CategoricalAction != ActionPurge {
		t.Errorf("CategoricalAction = %q; want purge", r.opts.CategoricalAction)
	}
}

// TestActionOffPlansNoMove is the observability-only mode: a rejected file is
// still classified and counted, but no move is planned, so nothing on disk
// changes. Counting it is the point -- an operator wants to see what the sweep
// WOULD do before letting it act.
func TestActionOffPlansNoMove(t *testing.T) {
	plan := planOne(t, overrunBody, func(o *Options) {
		o.MisSyncedAction = ActionOff
		o.CategoricalAction = ActionOff
	})

	if plan.Counts.MisSynced != 1 {
		t.Errorf("MisSynced count = %d; want 1 (off still classifies)", plan.Counts.MisSynced)
	}
	if len(plan.Moves) != 0 {
		t.Errorf("len(Moves) = %d; want 0 (off never mutates)", len(plan.Moves))
	}
	if len(plan.Findings) != 1 {
		t.Fatalf("len(Findings) = %d; want 1", len(plan.Findings))
	}
	if plan.Findings[0].Action != "" {
		t.Errorf("Finding.Action = %q; want empty (no action planned)", plan.Findings[0].Action)
	}
}

// TestQuarantineActionOnOverrunKeepsNoWords verifies "quarantine" on the
// MisSynced arm moves the file aside WITHOUT writing a .txt. That is exactly
// what distinguishes it from "demote"; conflating the two would either lose the
// words or write them when the operator asked not to.
func TestQuarantineActionOnOverrunKeepsNoWords(t *testing.T) {
	plan := planOne(t, overrunBody, func(o *Options) {
		o.MisSyncedAction = ActionQuarantine
	})

	if len(plan.Moves) != 1 {
		t.Fatalf("len(Moves) = %d; want 1", len(plan.Moves))
	}
	mv := plan.Moves[0]
	if mv.Kind != realign.KindQuarantine {
		t.Errorf("Kind = %q; want %q", mv.Kind, realign.KindQuarantine)
	}
	if mv.TextBody != "" || mv.TextPath != "" {
		t.Errorf("quarantine carried words (TextPath=%q); want none -- that is demote's job", mv.TextPath)
	}
}

// TestDemoteActionKeepsTheWords is the counterpart: demote must carry the plain
// words and a .txt destination, since a MisSynced lyric's words are correct.
func TestDemoteActionKeepsTheWords(t *testing.T) {
	plan := planOne(t, overrunBody, func(o *Options) {
		o.MisSyncedAction = ActionDemote
	})

	if len(plan.Moves) != 1 {
		t.Fatalf("len(Moves) = %d; want 1", len(plan.Moves))
	}
	mv := plan.Moves[0]
	if mv.Kind != realign.KindDemote {
		t.Errorf("Kind = %q; want %q", mv.Kind, realign.KindDemote)
	}
	if mv.TextBody == "" {
		t.Error("TextBody is empty; demote must carry the plain words")
	}
}

// TestPurgeActionOnOverrunDeletes verifies the MisSynced arm can be made
// irreversible on its own -- the combination the legacy flags could only reach
// by setting both --on-fail delete and --purge.
func TestPurgeActionOnOverrunDeletes(t *testing.T) {
	plan := planOne(t, overrunBody, func(o *Options) {
		o.MisSyncedAction = ActionPurge
	})

	if len(plan.Moves) != 1 {
		t.Fatalf("len(Moves) = %d; want 1", len(plan.Moves))
	}
	if plan.Moves[0].Kind != realign.KindPurge {
		t.Errorf("Kind = %q; want %q", plan.Moves[0].Kind, realign.KindPurge)
	}
}

// TestCategoricalActionOffLeavesTheFile verifies the categorical arm honors off
// independently of the MisSynced arm, so an operator can act on one verdict
// while only observing the other.
func TestCategoricalActionOffLeavesTheFile(t *testing.T) {
	plan := planOne(t, categoricalBody, func(o *Options) {
		o.MisSyncedAction = ActionDemote
		o.CategoricalAction = ActionOff
	})

	if plan.Counts.Categorical != 1 {
		t.Fatalf("Categorical count = %d; want 1", plan.Counts.Categorical)
	}
	if len(plan.Moves) != 0 {
		t.Errorf("len(Moves) = %d; want 0 (categorical action is off)", len(plan.Moves))
	}
}

// TestValidateRejectsAnUnknownAction verifies a bad action is refused before any
// file is touched, matching how Validate already refuses a bad --on-fail.
func TestValidateRejectsAnUnknownAction(t *testing.T) {
	if err := (Options{MisSyncedAction: "resync", QuarantineDir: "/q"}).Validate(); err == nil {
		t.Error("an unknown MisSyncedAction was accepted; want rejected")
	}
	if err := (Options{CategoricalAction: ActionDemote, QuarantineDir: "/q"}).Validate(); err == nil {
		t.Error("CategoricalAction=demote was accepted; want rejected (no words worth keeping)")
	}
}

// TestValidateRequiresQuarantineDirForAQuarantiningAction verifies the existing
// "a quarantine dir is required unless purging" rule follows the ACTIONS rather
// than only the legacy Purge flag. Without this, a sweep configured to
// quarantine with no directory would plan moves to a bare relative path.
func TestValidateRequiresQuarantineDirForAQuarantiningAction(t *testing.T) {
	if err := (Options{MisSyncedAction: ActionQuarantine, CategoricalAction: ActionQuarantine}).Validate(); err == nil {
		t.Error("a quarantining config with no QuarantineDir was accepted; want rejected")
	}
	// Neither arm quarantines, so no directory is needed.
	if err := (Options{MisSyncedAction: ActionPurge, CategoricalAction: ActionOff}).Validate(); err != nil {
		t.Errorf("a non-quarantining config was rejected: %v", err)
	}
}
