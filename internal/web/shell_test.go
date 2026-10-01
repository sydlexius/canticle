package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveStatic GETs one embedded asset through StaticHandler and returns its body.
func serveStatic(t *testing.T, path string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	StaticHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
	}
	return rec.Body.String(), rec
}

// TestShellScriptServed pins that shell.js is embedded and served as JavaScript,
// so the layout's script tag cannot 404 and leave the phone sidebar unreachable.
// It also pins the two structural choices the #1094 review found missing: every
// listener is delegated from document (htmx 2 history restore replaces the body,
// so a listener bound to the original toggle goes dead), and a restore is
// normalized to the closed state.
func TestShellScriptServed(t *testing.T) {
	js, rec := serveStatic(t, "/static/js/shell.js")
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a JavaScript type", ct)
	}
	for _, want := range []string{
		"mx-js",
		"document.addEventListener('click'",
		"closest(TOGGLE)",
		// The delegated click flips the CURRENT toggle (looked up at event time).
		"setOpen(t, !isOpen(t), true)",
		"htmx:historyRestore",
		"document.addEventListener('focusout'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("shell.js missing %q", want)
		}
	}
	if strings.Contains(js, "button.addEventListener") || strings.Contains(js, "nav.addEventListener") {
		t.Error("shell.js binds a listener to a node htmx history restore replaces; delegate from document")
	}
}

// TestShellPhoneCSSServed pins the served stylesheet's phone-width block. Without
// it the sidebar keeps its 220px fixed column at 390px and the content scrolls
// sideways, and without the .mx-js rule the toggle has nothing to open.
func TestShellPhoneCSSServed(t *testing.T) {
	css, _ := serveStatic(t, "/static/css/output.css")
	const media = "@media (max-width:767.98px)"
	i := strings.Index(css, media)
	if i < 0 {
		t.Fatalf("output.css has no %q block", media)
	}
	block := css[i:]
	for _, want := range []string{
		".mx-js .mx-sidebar{",
		"transform:translate(-100%)",
		"visibility:hidden",
		// The drawer starts below the top bar so the toggle stays tappable.
		"inset-block:var(--mx-topbar-height) 0",
		".mx-js .mx-sidebar[data-open=true]{visibility:visible",
		".mx-js .mx-topbar{",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("output.css phone block missing %q", want)
		}
	}
}

// TestShellPhoneSidebarToggle pins the #1094 markup contract: the top-bar button
// controls the sidebar by id and starts collapsed. The sidebar is NOT focusable
// in the markup (shell.js adds tabindex only while the drawer is open), so a
// desktop click on empty sidebar space cannot focus it.
func TestShellPhoneSidebarToggle(t *testing.T) {
	body := renderShell(t, false)
	for _, want := range []string{
		`class="mx-nav-toggle"`,
		`aria-expanded="false"`,
		`aria-controls="mx-sidebar"`,
		`id="mx-sidebar"`,
		`src="/static/js/shell.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell page missing %q", want)
		}
	}
	if strings.Contains(body, `data-open`) {
		t.Error("sidebar must render closed (no data-open) before JS runs")
	}
	if strings.Contains(body, `id="mx-sidebar" tabindex`) {
		t.Error("sidebar must not carry tabindex in the markup")
	}
}
