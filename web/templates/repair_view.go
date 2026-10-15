package templates

import "strings"

// Presentation models for the operator repair prompts and run page (#1543,
// epic #1537). Every field is a count, a fixed identifier, or static text a
// repair's author wrote (internal/repair.Info); none is ever read from the
// library or the database, so no view here can carry a path, artist, title or
// lyric line. The routes (#1544) map runner snapshots onto these structs; this
// package imports nothing from internal/repair so the templates stay
// presentation-only.

// RepairState is which of the run page's three states to render.
type RepairState string

const (
	// RepairConfirm asks the operator to confirm before anything runs.
	RepairConfirm RepairState = "confirm"
	// RepairRunning shows live progress and polls for updates.
	RepairRunning RepairState = "running"
	// RepairFinished shows the final counts.
	RepairFinished RepairState = "finished"
)

// RepairPromptView is the reusable fragment shown on the page a repair
// relates to. It renders nothing unless Count > 0 or a run is live.
type RepairPromptView struct {
	// Title and Description are the repair's static Info text.
	Title       string
	Description string
	// CostNote is Info.CostNote, shown whenever non-empty; ReadsFiles is
	// Info.ReadsFiles, which adds the files/spun-down sentence.
	CostNote   string
	ReadsFiles bool
	// Count is the pending count from Repair.Pending (database only).
	Count int64
	// Running is true while a run of this repair is live; Done and Total are
	// its progress (Total <= 0 means not known yet).
	Running bool
	Done    int64
	Total   int64
	// StartHref is the run page's confirm URL (a plain GET link).
	StartHref string
}

// Visible reports whether the fragment renders anything.
func (v RepairPromptView) Visible() bool { return v.Count > 0 || v.Running }

// RepairCountLine is one named count of a finished run. Key is the declared
// repair.CountKey; the template shows it with underscores as spaces.
type RepairCountLine struct {
	Key string
	N   int64
}

// Label is the count key as display text.
func (c RepairCountLine) Label() string { return strings.ReplaceAll(c.Key, "_", " ") }

// RepairRunView is the run page's view model.
type RepairRunView struct {
	State RepairState
	Title string
	// Description, CostNote, ReadsFiles and NeedsBackup are the repair's Info.
	Description string
	CostNote    string
	ReadsFiles  bool
	NeedsBackup bool
	// Pending is the count shown on the confirm state.
	Pending int64
	// Done and Total are the running state's progress (Total <= 0: unknown).
	Done  int64
	Total int64
	// Counts are the finished run's counts, in declared order.
	Counts []RepairCountLine
	// Failed and Canceled qualify the finished state. A failure shows no
	// error text: a repair's error may wrap a library path.
	Failed   bool
	Canceled bool
	// CSRFToken, StartAction and CancelAction drive the POST forms; PollHref
	// is the GET the running state polls; BackHref returns to the origin page.
	CSRFToken    string
	StartAction  string
	CancelAction string
	PollHref     string
	BackHref     string
}

// repairProgressText renders "done of total", or just "done" when the total
// is not known yet.
func repairProgressText(done, total int64) string {
	if total <= 0 {
		return itoa64(done) + " done"
	}
	return itoa64(done) + " of " + itoa64(total)
}
