package web

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/repair"
	"github.com/sydlexius/canticle/internal/trustnet"
)

// fakeRepair is a repair.Repair whose Pending and Run are scripted. Run blocks
// on gate (when set) until it is closed or the run is canceled.
type fakeRepair struct {
	key                 repair.Key
	pending             int64
	pendingErr, runErr  error
	gate                chan struct{}
	backup              bool // Info.NeedsBackup
	pendingCalls, calls atomic.Int32
}

func (f *fakeRepair) Info() repair.Info {
	return repair.Info{Key: f.key, Title: "Fix " + string(f.key), Description: "Fixes rows.", Counts: []repair.CountKey{"fixed"}, NeedsBackup: f.backup}
}

// startSpy records the Options of the last Start (on the handler's goroutine).
type startSpy struct {
	RepairRunner
	got repair.Options
}

func (s *startSpy) Start(key repair.Key, o repair.Options) (int64, error) {
	s.got = o
	return s.RepairRunner.Start(key, o)
}

func (f *fakeRepair) Pending(context.Context, repair.Options) (int64, error) {
	f.pendingCalls.Add(1)
	return f.pending, f.pendingErr
}

func (f *fakeRepair) Run(ctx context.Context, o repair.Options) (repair.Result, error) {
	f.calls.Add(1)
	res := repair.NewResult("fixed")
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return res, fmt.Errorf("fake: %w", ctx.Err())
		}
	}
	res.Add("fixed", 3)
	o.Report(3, 3)
	return res, f.runErr
}

// waitCalls waits up to 5s for a call counter that the runner's goroutine
// bumps to reach want.
func waitCalls(t *testing.T, n *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

const repairTok = "7777777777777777777777777777777777777777777777777777777777777777"

type repairRig struct {
	mux    *http.ServeMux
	sess   *http.Cookie
	runner *repair.Runner
	a, b   *fakeRepair
	deps   RepairDeps
}

// newRepairRig builds a guarded UI with fix-a (5 pending) and fix-b (2
// pending) registered; attach=false leaves AttachRepairs uncalled. Each
// tweak edits the rig (its deps included) before they are attached.
func newRepairRig(t *testing.T, attach bool, tweaks ...func(*repairRig)) *repairRig {
	t.Helper()
	au, svc := newTestAuth(t, trustnet.LoopbackOnly())
	g := &repairRig{mux: http.NewServeMux(), sess: &http.Cookie{Name: SessionCookieName, Value: loginToken(t, svc)},
		a: &fakeRepair{key: "fix-a", pending: 5}, b: &fakeRepair{key: "fix-b", pending: 2}}
	reg := repair.NewRegistry()
	for _, f := range []*fakeRepair{g.a, g.b} {
		if err := reg.Register(f); err != nil {
			t.Fatal(err)
		}
	}
	g.runner = repair.NewRunner(reg, nil)
	t.Cleanup(func() { _ = g.runner.Close() }) // also waits out the run's final log line
	g.deps = RepairDeps{Registry: reg, Runner: g.runner, DBPath: filepath.Join(t.TempDir(), "x.db")}
	for _, tw := range tweaks {
		tw(g)
	}
	ui := NewUI(config.Config{}, "vtest", WithAuth(au))
	if attach {
		ui.AttachRepairs(g.deps)
	}
	ui.Register(g.mux)
	return g
}

// do sends a request with the session, a same-origin Origin and the CSRF
// cookie; hdr overrides or adds headers ("" deletes one).
func (g *repairRig) do(method, path string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.RemoteAddr = "198.51.100.40:1"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	req.AddCookie(g.sess)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: repairTok})
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	if hdr["Cookie"] == "none" {
		req.Header.Del("Cookie")
	}
	rec := httptest.NewRecorder()
	g.mux.ServeHTTP(rec, req)
	return rec
}

var repairCSRF = url.Values{"csrf_token": {repairTok}}

// waitState polls the runner until its snapshot reaches want.
func (g *repairRig) waitState(t *testing.T, want repair.State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if g.runner.Snapshot().State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run never reached %s (now %s)", want, g.runner.Snapshot().State)
}

func TestRepairRoutesAreGuarded(t *testing.T) {
	a, _ := newTestAuth(t, trustnet.LoopbackOnly())
	ui := NewUI(config.Config{}, "vtest", WithAuth(a))
	ui.Register(http.NewServeMux())
	for _, pat := range []string{"GET /repairs/{key}", "POST /repairs/{key}", "GET /repairs/{key}/status", "POST /repairs/{key}/cancel"} {
		if !slices.Contains(ui.guardedRoutes, pat) {
			t.Errorf("%q is not registered through the session guard", pat)
		}
	}
}

func TestRepairPostsEnforceSameOriginAndCSRF(t *testing.T) {
	bad := map[string]struct {
		form url.Values
		hdr  map[string]string
	}{
		"cross-site":      {repairCSRF, map[string]string{"Origin": "http://evil.example"}},
		"no csrf field":   {url.Values{}, nil},
		"csrf mismatch":   {url.Values{"csrf_token": {strings.Repeat("8", 64)}}, nil},
		"sec-fetch cross": {repairCSRF, map[string]string{"Sec-Fetch-Site": "cross-site"}},
	}
	for name, c := range bad {
		t.Run("start/"+name, func(t *testing.T) {
			g := newRepairRig(t, true)
			if rec := g.do(http.MethodPost, "/repairs/fix-a", c.form, c.hdr); rec.Code != http.StatusForbidden {
				t.Errorf("start = %d, want 403", rec.Code)
			}
			if g.a.pendingCalls.Load()+g.a.calls.Load() != 0 || g.runner.Snapshot().State != repair.StateIdle {
				t.Error("a refused start reached the repair or the runner")
			}
		})
		t.Run("cancel/"+name, func(t *testing.T) {
			g := newRepairRig(t, true)
			g.a.gate = make(chan struct{})
			defer close(g.a.gate)
			if _, err := g.runner.Start("fix-a", repair.Options{Apply: true}); err != nil {
				t.Fatal(err)
			}
			if rec := g.do(http.MethodPost, "/repairs/fix-a/cancel", c.form, c.hdr); rec.Code != http.StatusForbidden {
				t.Errorf("cancel = %d, want 403", rec.Code)
			}
			time.Sleep(20 * time.Millisecond)
			if s := g.runner.Snapshot(); s.State != repair.StateRunning {
				t.Errorf("a refused cancel stopped the run: %s", s.State)
			}
		})
	}
}

func TestRepairUnknownOrUnattached404(t *testing.T) {
	routes := []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodPost, ""}, {http.MethodGet, "/status"}, {http.MethodPost, "/cancel"}}
	cases := []struct {
		name, key string
		attach    bool
	}{{"unattached", "fix-a", false}, {"unknown key", "fix-z", true}, {"malformed key", "Fix_A", true}}
	for _, c := range cases {
		g := newRepairRig(t, c.attach)
		for _, rt := range routes {
			if rec := g.do(rt.method, "/repairs/"+c.key+rt.suffix, repairCSRF, nil); rec.Code != http.StatusNotFound {
				t.Errorf("%s: %s /repairs/%s%s = %d, want 404", c.name, rt.method, c.key, rt.suffix, rec.Code)
			}
		}
		if g.a.pendingCalls.Load()+g.a.calls.Load() != 0 {
			t.Errorf("%s: the repair ran", c.name)
		}
	}
}

func TestRepairStartWhileBusyIsRefused(t *testing.T) {
	g := newRepairRig(t, true)
	g.a.gate = make(chan struct{})
	defer close(g.a.gate)
	logs := captureLogs(t)
	if rec := g.do(http.MethodPost, "/repairs/fix-a", repairCSRF, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("first start = %d, want 303", rec.Code)
	}
	// A double-submitted start of the live repair lands on its own run.
	if again := g.do(http.MethodPost, "/repairs/fix-a", repairCSRF, nil); again.Code != http.StatusSeeOther ||
		again.Header().Get("Location") != "/repairs/fix-a?run=1" {
		t.Errorf("second start of the live repair = %d -> %q, want 303 to its run", again.Code, again.Header().Get("Location"))
	}
	rec := g.do(http.MethodPost, "/repairs/fix-b", repairCSRF, nil)
	if rec.Code != http.StatusConflict || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
		!strings.Contains(rec.Body.String(), "Another repair is already running") {
		t.Errorf("busy start = %d %q %q, want a plain 409", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	// Run is called on the runner's goroutine, so the start's 303 can land
	// before fix-a's call is counted: wait for it rather than read it once.
	waitCalls(t, &g.a.calls, 1)
	if g.b.calls.Load() != 0 || g.a.calls.Load() != 1 {
		t.Errorf("runs: fix-a %d, fix-b %d, want 1 and 0", g.a.calls.Load(), g.b.calls.Load())
	}
	if !strings.Contains(logs.String(), `reason=busy error_class="repair: busy"`) {
		t.Errorf("busy refusal log lacks its class:\n%s", logs)
	}
}

// TestRepairRunPageWhileLive: the page the start redirects to shows the live
// run polling, with no Start form.
func TestRepairRunPageWhileLive(t *testing.T) {
	g := newRepairRig(t, true)
	g.a.gate = make(chan struct{})
	defer close(g.a.gate)
	if rec := g.do(http.MethodPost, "/repairs/fix-a", repairCSRF, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("start = %d, want 303", rec.Code)
	}
	body := g.do(http.MethodGet, "/repairs/fix-a?run=1", nil, nil).Body.String()
	if !strings.Contains(body, `hx-trigger="every 2s"`) || !strings.Contains(body, "Running:") ||
		strings.Contains(body, `action="/repairs/fix-a"`) || strings.Contains(body, "The run finished") {
		t.Errorf("run page while live does not show the running state:\n%s", body)
	}
}

// TestRepairStartBackup: a repair that needs a backup applies with one in the
// database's folder named for the key; with no database path it is refused.
func TestRepairStartBackup(t *testing.T) {
	spy := &startSpy{}
	g := newRepairRig(t, true, func(g *repairRig) {
		g.a.backup = true
		spy.RepairRunner = g.deps.Runner
		g.deps.Runner = spy
	})
	if rec := g.do(http.MethodPost, "/repairs/fix-a", repairCSRF, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("start = %d %q, want 303", rec.Code, rec.Body.String())
	}
	if o := spy.got; !o.Apply || filepath.Dir(o.Backup) != filepath.Dir(g.deps.DBPath) ||
		!strings.Contains(filepath.Base(o.Backup), "fix-a") {
		t.Errorf("start options = apply %v backup %q, want apply with a fix-a backup beside %q", o.Apply, o.Backup, g.deps.DBPath)
	}

	g = newRepairRig(t, true, func(g *repairRig) { g.a.backup, g.deps.DBPath = true, "" })
	rec := g.do(http.MethodPost, "/repairs/fix-a", repairCSRF, nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
		!strings.Contains(rec.Body.String(), "needs a backup location") {
		t.Errorf("start with no backup location = %d %q, want a plain 503", rec.Code, rec.Body.String())
	}
}

func TestRepairStartAfterCloseIs503(t *testing.T) {
	g := newRepairRig(t, true)
	logs := captureLogs(t)
	if err := g.runner.Close(); err != nil {
		t.Fatal(err)
	}
	rec := g.do(http.MethodPost, "/repairs/fix-a", repairCSRF, nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "shutting down") {
		t.Errorf("start after Close = %d %q, want 503", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), `reason=closed error_class="repair: runner closed"`) {
		t.Errorf("closed refusal log lacks its class:\n%s", logs)
	}
}

// TestRepairPollOtherRepairsRun: once another repair's run replaces the
// snapshot, this repair's poll says no run of it is live, not that it finished.
func TestRepairPollOtherRepairsRun(t *testing.T) {
	g := newRepairRig(t, true)
	if _, err := g.runner.Start("fix-b", repair.Options{Apply: true}); err != nil {
		t.Fatal(err)
	}
	g.waitState(t, repair.StateDone)
	body := g.do(http.MethodGet, "/repairs/fix-a/status", nil, nil).Body.String()
	if !strings.Contains(body, "No run of this repair is live.") || strings.Contains(body, "The run finished") ||
		strings.Contains(body, "hx-trigger") {
		t.Errorf("fix-a poll after fix-b's run:\n%s", body)
	}
}

func TestRepairStartWithNothingPendingIsRefused(t *testing.T) {
	g := newRepairRig(t, true)
	g.a.pending = 0
	rec := g.do(http.MethodPost, "/repairs/fix-a", repairCSRF, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Nothing is pending") {
		t.Errorf("zero-pending start = %d %q, want 409", rec.Code, rec.Body.String())
	}
	time.Sleep(20 * time.Millisecond)
	if g.a.calls.Load() != 0 || g.runner.Snapshot().State != repair.StateIdle {
		t.Error("a start with nothing pending ran the repair")
	}
}

func TestRepairConfirmShowsPendingAndNeverRuns(t *testing.T) {
	g := newRepairRig(t, true)
	rec := g.do(http.MethodGet, "/repairs/fix-a", nil, nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "<strong>5</strong> pending") ||
		!strings.Contains(body, `action="/repairs/fix-a"`) || !strings.Contains(body, repairTok) {
		t.Errorf("confirm = %d, want the pending count and a start form:\n%s", rec.Code, body)
	}
	if g.a.pendingCalls.Load() != 1 || g.a.calls.Load() != 0 {
		t.Errorf("confirm called Pending %d and Run %d times, want 1 and 0", g.a.pendingCalls.Load(), g.a.calls.Load())
	}
}

func TestRepairStartPollAndCancel(t *testing.T) {
	g := newRepairRig(t, true)
	// Idle: the poll is the finished fragment, never an error, and stops polling.
	rec := g.do(http.MethodGet, "/repairs/fix-a/status", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="mx-repair-run"`) ||
		strings.Contains(rec.Body.String(), "hx-trigger") || strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("idle poll = %d %q, want the finished fragment", rec.Code, rec.Body.String())
	}
	g.a.gate = make(chan struct{})
	start := g.do(http.MethodPost, "/repairs/fix-a", repairCSRF, nil)
	if start.Code != http.StatusSeeOther || start.Header().Get("Location") != "/repairs/fix-a?run=1" {
		t.Fatalf("start = %d -> %q, want 303 to the run page", start.Code, start.Header().Get("Location"))
	}
	if s := g.runner.Snapshot(); !liveFor(s, "fix-a") || !s.Apply {
		t.Fatalf("start did not begin an applying run: %+v", s)
	}
	if body := g.do(http.MethodGet, "/repairs/fix-a/status", nil, nil).Body.String(); !strings.Contains(body, `hx-trigger="every 2s"`) {
		t.Errorf("live poll does not keep polling:\n%s", body)
	}
	// Cancel of the other repair leaves this run alone.
	if c := g.do(http.MethodPost, "/repairs/fix-b/cancel", repairCSRF, nil); c.Header().Get("Location") != "/repairs/fix-b" {
		t.Errorf("cancel with no live run of fix-b -> %q, want its plain page", c.Header().Get("Location"))
	}
	time.Sleep(50 * time.Millisecond) // a wrongful cancel ends the run within this
	if !liveFor(g.runner.Snapshot(), "fix-a") {
		t.Fatal("canceling fix-b stopped fix-a's run")
	}
	if c := g.do(http.MethodPost, "/repairs/fix-a/cancel", repairCSRF, nil); c.Code != http.StatusSeeOther ||
		c.Header().Get("Location") != "/repairs/fix-a?run=1" {
		t.Errorf("cancel = %d -> %q, want 303 to the run", c.Code, c.Header().Get("Location"))
	}
	g.waitState(t, repair.StateCanceled)
	body := g.do(http.MethodGet, "/repairs/fix-a/status", nil, nil).Body.String()
	if !strings.Contains(body, "The run was canceled") || strings.Contains(body, "hx-trigger") {
		t.Errorf("poll after cancel:\n%s", body)
	}
	if page := g.do(http.MethodGet, "/repairs/fix-a?run=1", nil, nil).Body.String(); !strings.Contains(page, "The run was canceled") {
		t.Errorf("run page for the finished run does not show it:\n%s", page)
	}
}

func TestRepairPollExpiredSessionUnderHTMX(t *testing.T) {
	g := newRepairRig(t, true)
	rec := g.do(http.MethodGet, "/repairs/fix-a/status", nil, map[string]string{
		"Cookie": "none", "HX-Request": "true", "HX-Current-URL": "http://example.com/repairs/fix-a?run=1"})
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" ||
		rec.Header().Get("HX-Redirect") != "/login?next="+url.QueryEscape("/repairs/fix-a?run=1") {
		t.Errorf("htmx poll without a session = %d HX-Redirect %q Location %q, want 200 + HX-Redirect to login",
			rec.Code, rec.Header().Get("HX-Redirect"), rec.Header().Get("Location"))
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Vary") != "HX-Request" {
		t.Errorf("htmx redirect Cache-Control %q Vary %q, want no-store and HX-Request",
			rec.Header().Get("Cache-Control"), rec.Header().Get("Vary"))
	}
	// htmx's history-restore loader ignores HX-Redirect and swaps any 2xx
	// body in; it must get the 303 so the page is not blanked.
	restore := g.do(http.MethodGet, "/repairs/fix-a", nil, map[string]string{
		"Cookie": "none", "HX-Request": "true", "HX-History-Restore-Request": "true"})
	if restore.Code != http.StatusSeeOther || !strings.HasPrefix(restore.Header().Get("Location"), "/login") ||
		restore.Header().Get("HX-Redirect") != "" {
		t.Errorf("history restore without a session = %d HX-Redirect %q, want 303 /login",
			restore.Code, restore.Header().Get("HX-Redirect"))
	}
	plain := g.do(http.MethodGet, "/repairs/fix-a/status", nil, map[string]string{"Cookie": "none"})
	if plain.Code != http.StatusSeeOther || !strings.HasPrefix(plain.Header().Get("Location"), "/login") {
		t.Errorf("plain request without a session = %d, want 303 /login", plain.Code)
	}
}

// TestRepairFailuresLeakNoPath feeds a Pending error and a Run error whose text
// holds a sentinel path, and asserts the path reaches no log line and no
// response while the log lines still carry the key and the counts.
func TestRepairFailuresLeakNoPath(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "Hidden Sentinel Title.flac")
	leaky := fmt.Errorf("row 9: %w", &fs.PathError{Op: "open", Path: secret, Err: syscall.EACCES})
	g := newRepairRig(t, true)
	logs := captureLogs(t) // locked: the run's goroutine logs too
	var out strings.Builder
	g.a.pendingErr = leaky
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		rec := g.do(m, "/repairs/fix-a", repairCSRF, nil)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s with a failing Pending = %d, want 500", m, rec.Code)
		}
		out.WriteString(rec.Body.String())
	}
	g.a.pendingErr, g.a.runErr = nil, leaky
	out.WriteString(g.do(http.MethodPost, "/repairs/fix-a", repairCSRF, nil).Header().Get("Location"))
	g.waitState(t, repair.StateFailed)
	out.WriteString(g.do(http.MethodGet, "/repairs/fix-a/status", nil, nil).Body.String())
	out.WriteString(g.do(http.MethodGet, "/repairs/fix-a?run=1", nil, nil).Body.String())
	// The run's final log line follows its Finished state; Close waits for it.
	if err := g.runner.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `msg="repair: run finished" repair=fix-a state=failed`) {
		t.Errorf("the run's own log line is missing:\n%s", logs)
	}

	for _, s := range []string{secret, filepath.Dir(secret), "Hidden Sentinel Title"} {
		if strings.Contains(logs.String(), s) || strings.Contains(out.String(), s) {
			t.Fatalf("the error text leaked:\nlogs:\n%s\nresponses:\n%s", logs, out.String())
		}
	}
	if !strings.Contains(out.String(), "The run failed") {
		t.Error("the failed run is not reported as failed")
	}
	if !strings.Contains(logs.String(), `msg="web: repair started" event=repair_start repair=fix-a pending=5 run=1`) ||
		!strings.Contains(logs.String(), `repair=fix-a error_class="permission denied"`) {
		t.Errorf("log lines do not carry the key and counts:\n%s", logs)
	}
}

// TestRepairTokenFailureAnswers500 forces CSRF token generation to fail on a
// request that carries no CSRF cookie and asserts every route that would embed
// the token answers a plain 500 with no form (a token-less form would post an
// empty token and fail with a bare 403). The finished status fragment holds no
// form, so it still answers 200.
func TestRepairTokenFailureAnswers500(t *testing.T) {
	g := newRepairRig(t, true)
	g.a.gate = make(chan struct{})
	t.Cleanup(func() { close(g.a.gate) })
	if rec := g.do(http.MethodPost, "/repairs/fix-a", repairCSRF, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("start = %d, want 303", rec.Code)
	}
	orig := csrfRandRead
	csrfRandRead = func([]byte) (int, error) { return 0, fmt.Errorf("entropy: %s", "/secret/path") }
	t.Cleanup(func() { csrfRandRead = orig })
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "198.51.100.40:1"
		req.AddCookie(g.sess) // no CSRF cookie: a token must be minted
		rec := httptest.NewRecorder()
		g.mux.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/repairs/fix-a", "/repairs/fix-a/status", "/repairs/fix-b"} {
		rec := get(path)
		body := rec.Body.String()
		if rec.Code != http.StatusInternalServerError || strings.Contains(body, "<form") ||
			strings.Contains(body, "csrf_token") || strings.Contains(body, "/secret/path") {
			t.Errorf("GET %s = %d, want a plain 500 with no form or error text:\n%s", path, rec.Code, body)
		}
	}
	// The finished state carries no form and so needs no token.
	if rec := get("/repairs/fix-b/status"); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "<form") {
		t.Errorf("finished status = %d, want 200 with no form", rec.Code)
	}
}
