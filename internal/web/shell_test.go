package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestShellScriptServed pins that shell.js is embedded and served as JavaScript,
// so the layout's script tag cannot 404 and leave the phone sidebar unreachable.
func TestShellScriptServed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/static/js/shell.js", nil)
	rec := httptest.NewRecorder()
	StaticHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET shell.js status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a JavaScript type", ct)
	}
	if !strings.Contains(rec.Body.String(), "mx-js") {
		t.Error("shell.js does not add the mx-js enhancement class")
	}
}

// TestShellPhoneSidebarToggle pins the #1094 markup contract: the top-bar button
// controls the sidebar by id, starts collapsed, and the sidebar is focusable so
// shell.js can move focus into it. The script is loaded deferred from /static.
func TestShellPhoneSidebarToggle(t *testing.T) {
	body := renderShell(t, false)
	for _, want := range []string{
		`class="mx-nav-toggle"`,
		`aria-expanded="false"`,
		`aria-controls="mx-sidebar"`,
		`id="mx-sidebar"`,
		`tabindex="-1"`,
		`src="/static/js/shell.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell page missing %q", want)
		}
	}
	if strings.Contains(body, `data-open`) {
		t.Error("sidebar must render closed (no data-open) before JS runs")
	}
}
