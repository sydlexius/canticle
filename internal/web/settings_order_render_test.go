package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// orderListItemRE captures the value of each rendered order-list row, in
// document order.
var orderListItemRE = regexp.MustCompile(`<li[^>]*class="[^"]*mx-orderlist-item[^"]*"[^>]*data-value="([^"]+)"`)

// orderCfg is a config whose fallback order is deliberately NOT the registry's
// declaration order, so a render that ignores the stored order is visible.
func orderCfg() config.Config {
	cfg := config.Config{}
	cfg.Providers.FallbackOrder = []string{"petitlyrics", "musixmatch"}
	return cfg
}

// renderSettings returns the rendered /settings body.
func renderSettings(t *testing.T, cfg config.Config) string {
	t.Helper()
	mux := newUIServer(cfg, "v0")
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// TestFallbackOrderRendersAsOrderList is the core #837 assertion: the provider
// order control is an ORDER LIST whose rows carry their value in document order,
// not a checkbox list. A checkbox list cannot express a permutation -- the
// browser submits checked boxes in DOM order, and the DOM order was the stored
// order, so every save wrote back the order it started with (a fixed point).
func TestFallbackOrderRendersAsOrderList(t *testing.T) {
	body := renderSettings(t, orderCfg())

	got := []string{}
	for _, m := range orderListItemRE.FindAllStringSubmatch(body, -1) {
		got = append(got, m[1])
	}
	want := []string{"petitlyrics", "musixmatch"}
	if len(got) != len(want) {
		t.Fatalf("rendered %d order-list rows %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q (document order must match the stored order)", i, got[i], want[i])
		}
	}
}

// TestFallbackOrderRowsCarrySubmittableValue asserts each row carries a hidden
// input under the field's config path. The submitted order is therefore the
// document order of the rows, which is what makes a reorder reach the server:
// the existing write path joins the repeated "value" fields in arrival order.
func TestFallbackOrderRowsCarrySubmittableValue(t *testing.T) {
	body := renderSettings(t, orderCfg())

	for _, p := range []string{"petitlyrics", "musixmatch"} {
		want := `<input type="hidden" name="providers.fallback_order" value="` + p + `"`
		if !strings.Contains(body, want) {
			t.Errorf("no hidden input carrying %q; the row's value must be submittable", p)
		}
	}
}

// TestFallbackOrderHasKeyboardReorderControls asserts every row exposes
// move-up/move-down buttons. Drag-and-drop alone is unusable without a pointer,
// so the keyboard path is the accessible equivalent, not a nicety.
func TestFallbackOrderHasKeyboardReorderControls(t *testing.T) {
	body := renderSettings(t, orderCfg())

	for _, dir := range []string{"up", "down"} {
		if !strings.Contains(body, `data-order-move="`+dir+`"`) {
			t.Errorf("no move-%s control rendered; keyboard reordering must not depend on drag", dir)
		}
	}
	if n := strings.Count(body, `data-order-move=`); n != 4 {
		t.Errorf("got %d move controls, want 4 (up+down for each of 2 providers)", n)
	}
}

// TestFallbackOrderIsNotACheckboxList guards the regression directly: the order
// control must not render provider checkboxes, which is what made it
// indistinguishable from the separate "which sources to use" enablement list
// above it (providers.disabled). Membership is that field's job.
func TestFallbackOrderIsNotACheckboxList(t *testing.T) {
	body := renderSettings(t, orderCfg())

	if strings.Contains(body, `type="checkbox"`+"\n"+`					class="mx-settings-checkbox"`+"\n"+`					name="providers.fallback_order"`) {
		t.Error("fallback_order still renders checkboxes")
	}
	// The robust form of the same check: no checkbox input anywhere carries the
	// fallback_order name.
	checkboxRE := regexp.MustCompile(`<input[^>]*type="checkbox"[^>]*name="providers\.fallback_order"`)
	altRE := regexp.MustCompile(`<input[^>]*name="providers\.fallback_order"[^>]*type="checkbox"`)
	if checkboxRE.MatchString(body) || altRE.MatchString(body) {
		t.Error("fallback_order renders a checkbox input; it must be an order list, not an enablement list")
	}
}
