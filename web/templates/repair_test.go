package templates

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func renderRepair(t *testing.T, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// externalScript matches the layout's own <script src=...> tags, which the CSP
// allows (script-src 'self'). Only src plus an optional defer is excused: a tag
// with any other attribute (an onload handler, say) stays in the output and is
// checked.
var externalScript = regexp.MustCompile(`<script src="[^"]*"( defer)?></script>`)

// inlineForbidden matches CSP-forbidden constructs in lowercased output:
// script/style elements, style attributes, event-handler attributes and
// javascript: URLs.
var inlineForbidden = regexp.MustCompile(`<script|<style|\sstyle\s*=|\son[a-z]+\s*=|javascript:`)

// inlineViolation returns the forbidden construct found in out, or "".
func inlineViolation(out string) string {
	out = strings.ToLower(out)
	out = externalScript.ReplaceAllString(out, "")
	return inlineForbidden.FindString(out)
}

// noInline asserts the CSP-forbidden constructs are absent.
func noInline(t *testing.T, out string) {
	t.Helper()
	if bad := inlineViolation(out); bad != "" {
		t.Errorf("output contains %q:\n%s", bad, out)
	}
}

// TestNoInlineHelper proves the helper itself catches each bypass shape.
func TestNoInlineHelper(t *testing.T) {
	cases := []struct {
		name, in string
		bad      bool
	}{
		{"spaced style", `<div style = "a">`, true},
		{"upper style", `<div STYLE="a">`, true},
		{"newline style", "<div\nstyle=\"a\">", true},
		{"upper script", `<SCRIPT>x</SCRIPT>`, true},
		{"javascript url", `<a href="javascript:x">`, true},
		{"event handler", `<a onmouseover="x">`, true},
		{"external script allowed", `<script src="/static/js/htmx.min.js"></script><p class="a">ok</p>`, false},
		{"deferred external script allowed", `<script src="/static/js/htmx.min.js" defer></script>`, false},
		{"external script with a handler", `<script src="/static/js/htmx.min.js" onload="alert(1)"></script>`, true},
		{"deferred external script with a handler", `<script src="/static/js/htmx.min.js" defer onload="alert(1)"></script>`, true},
		{"clean control", `<div class="a"><p>hello, honest text</p></div>`, false},
	}
	for _, c := range cases {
		if got := inlineViolation(c.in) != ""; got != c.bad {
			t.Errorf("%s: violation=%v, want %v", c.name, got, c.bad)
		}
	}
}

func promptView() RepairPromptView {
	return RepairPromptView{Title: "Tag re-read", Description: "Re-reads stale tag rows.", ReadsFiles: true,
		CostNote: "Walks the whole library.", Count: 12, StartHref: "/repairs/tag-reread"}
}

func TestRepairPromptZeroRendersNothing(t *testing.T) {
	v := promptView()
	v.Count = 0
	if out := renderRepair(t, RepairPrompt(v)); out != "" {
		t.Errorf("zero count, no run: want empty, got %q", out)
	}
	v.Count = -3
	if out := renderRepair(t, RepairPrompt(v)); out != "" {
		t.Errorf("negative count, no run: want empty, got %q", out)
	}
}

func TestRepairPromptPending(t *testing.T) {
	out := renderRepair(t, RepairPrompt(promptView()))
	for _, want := range []string{"<strong>12</strong> pending", "Re-reads stale tag rows.", `href="/repairs/tag-reread"`,
		"wake a spun-down disk array", "Walks the whole library."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	noInline(t, out)
}

func TestRepairPromptCostWarningOnlyWhenReadsFiles(t *testing.T) {
	v := promptView()
	v.ReadsFiles = false
	out := renderRepair(t, RepairPrompt(v))
	if strings.Contains(out, "spun-down") {
		t.Errorf("files sentence shown for a repair that reads no files: %s", out)
	}
	if !strings.Contains(out, "Walks the whole library.") {
		t.Errorf("CostNote lost when ReadsFiles is false: %s", out)
	}
	v.CostNote = ""
	if out := renderRepair(t, RepairPrompt(v)); strings.Contains(out, "mx-repair-cost") {
		t.Errorf("empty cost block rendered: %s", out)
	}
	v.ReadsFiles, v.CostNote = false, "Walks the whole library."
	// The warning is independent of an empty CostNote.
	v.ReadsFiles = true
	v.CostNote = ""
	if out := renderRepair(t, RepairPrompt(v)); !strings.Contains(out, "spun-down") {
		t.Errorf("reads-files repair without a note lost the warning: %s", out)
	}
}

func TestRepairPromptRunning(t *testing.T) {
	v := promptView()
	v.Count = 0
	v.Running, v.Done, v.Total = true, 5, 20
	out := renderRepair(t, RepairPrompt(v))
	if !strings.Contains(out, "running, 5 of 20.") {
		t.Errorf("running line missing: %s", out)
	}
	if strings.Contains(out, "pending") {
		t.Errorf("running fragment shows the prompt: %s", out)
	}
	noInline(t, out)
	v.Total = 0
	if out := renderRepair(t, RepairPrompt(v)); !strings.Contains(out, "running, 5 done.") {
		t.Errorf("unknown total: %s", out)
	}
}

func runView(state RepairState) RepairRunView {
	return RepairRunView{State: state, Title: "Tag re-read", Description: "Re-reads stale tag rows.", ReadsFiles: true,
		CostNote: "Walks the whole library.", NeedsBackup: true, Pending: 7, CSRFToken: "tok",
		StartAction: "/repairs/tag-reread/start", CancelAction: "/repairs/tag-reread/cancel",
		PollHref: "/repairs/tag-reread/poll", BackHref: "/dashboard"}
}

func TestRepairRunPageConfirm(t *testing.T) {
	out := renderRepair(t, RepairRunPage("v", runView(RepairConfirm), nil, false, false))
	for _, want := range []string{"<strong>7</strong> pending", `action="/repairs/tag-reread/start"`, `value="tok"`,
		"spun-down", "backup file", ">Start</button>", `href="/dashboard"`} {
		if !strings.Contains(out, want) {
			t.Errorf("confirm: missing %q", want)
		}
	}
	if strings.Contains(out, "hx-trigger") {
		t.Error("confirm state must not poll")
	}
	noInline(t, out)
	v := runView(RepairConfirm)
	v.ReadsFiles, v.NeedsBackup = false, false
	out = renderRepair(t, RepairRunBody(v))
	if strings.Contains(out, "spun-down") || strings.Contains(out, "backup file") {
		t.Errorf("warnings shown without flags: %s", out)
	}
	if !strings.Contains(out, "Walks the whole library.") {
		t.Errorf("CostNote lost when ReadsFiles is false: %s", out)
	}
}

func TestRepairRunPageConfirmNothingToDo(t *testing.T) {
	v := runView(RepairConfirm)
	v.Pending = 0
	out := renderRepair(t, RepairRunBody(v))
	if strings.Contains(out, "<form") || strings.Contains(out, ">Start<") {
		t.Errorf("Start form offered with nothing pending: %s", out)
	}
	if !strings.Contains(out, "Nothing to do.") {
		t.Errorf("missing nothing-to-do line: %s", out)
	}
}

func TestRepairRunPageRunning(t *testing.T) {
	v := runView(RepairRunning)
	v.Done, v.Total = 3, 9
	out := renderRepair(t, RepairRunPage("v", v, nil, false, false))
	for _, want := range []string{`hx-get="/repairs/tag-reread/poll"`, `hx-trigger="every 2s"`, `hx-swap="outerHTML"`,
		"Running: 3 of 9.", `aria-label="Repair progress"`, `max="9"`, `value="3"`, `action="/repairs/tag-reread/cancel"`} {
		if !strings.Contains(out, want) {
			t.Errorf("running: missing %q", want)
		}
	}
	noInline(t, out)
	v.Total = 0
	out = renderRepair(t, RepairRunBody(v))
	if strings.Contains(out, "<progress") || !strings.Contains(out, "Running: 3 done.") {
		t.Errorf("unknown total: %s", out)
	}
}

func TestRepairRunPageFinished(t *testing.T) {
	v := runView(RepairFinished)
	v.Counts = []RepairCountLine{{Key: "rows_reset", N: 4}, {Key: "skipped", N: 0}}
	out := renderRepair(t, RepairRunPage("v", v, nil, false, false))
	for _, want := range []string{"The run finished.", "<dt>rows reset</dt>", "<dd>4</dd>", "<dd>0</dd>"} {
		if !strings.Contains(out, want) {
			t.Errorf("finished: missing %q", want)
		}
	}
	if strings.Contains(out, "hx-trigger") {
		t.Error("finished state must stop polling")
	}
	noInline(t, out)

	v.Failed = true
	if out := renderRepair(t, RepairRunBody(v)); !strings.Contains(out, "The run failed.") || strings.Contains(out, "finished.") {
		t.Errorf("failed: %s", out)
	}
	v.Failed, v.Canceled = false, true
	if out := renderRepair(t, RepairRunBody(v)); !strings.Contains(out, "was canceled") {
		t.Errorf("canceled: %s", out)
	}
}

// TestRepairViewsEscape confirms static text is HTML-escaped.
func TestRepairViewsEscape(t *testing.T) {
	v := promptView()
	v.Title = "<b>x</b>"
	if out := renderRepair(t, RepairPrompt(v)); strings.Contains(out, "<b>x</b>") {
		t.Errorf("title not escaped: %s", out)
	}
}

// Every URL a view carries goes through templ's own sanitizer: a javascript:
// value never reaches an href or a form action.
func TestRepairURLsAreSanitized(t *testing.T) {
	const bad = "javascript:alert(1)"
	p := promptView()
	p.StartHref = bad
	outs := []string{renderRepair(t, RepairPrompt(p))}
	p.Running, p.Total = true, 9
	outs = append(outs, renderRepair(t, RepairPrompt(p)))
	for _, s := range []RepairState{RepairConfirm, RepairRunning} {
		v := runView(s)
		v.StartAction, v.CancelAction, v.BackHref = bad, bad, bad
		outs = append(outs, renderRepair(t, RepairRunPage("v", v, nil, false, false)))
	}
	for _, out := range outs {
		if strings.Contains(strings.ToLower(out), "javascript:") {
			t.Errorf("a javascript: URL was rendered:\n%s", out)
		}
	}
}
