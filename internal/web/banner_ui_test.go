package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// spicetifyFAQURL was once linked from the banner as a way to obtain a Musixmatch
// token by hand (#385). It is now asserted ABSENT: serve mode provisions a token
// automatically and retries on a later start, so the banner's actionable remedies
// are Settings and the tokenless provider. Walking an operator through extracting
// a credential out of another application's traffic is not guidance canticle
// ships, and this constant is retained purely so the assertion below keeps the
// link from being reintroduced.
const spicetifyFAQURL = "https://spicetify.app/docs/faq#sometimes-popup-lyrics-andor-lyrics-plus-seem-to-not-work"

// renderShell mounts a (public, no-auth) UI and fetches the Reports workspace
// shell page, which renders through the shared Layout where the banner lives.
// /reports needs no data source on the no-key path (it shows the placeholder),
// so it exercises the shell without wiring a database.
func renderShell(t *testing.T, inactive bool) string {
	t.Helper()
	mux := http.NewServeMux()
	NewUI(config.Config{}, "v-test", WithMusixmatchInactive(inactive)).Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/reports", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /reports status = %d; want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestMusixmatchInactiveBannerRendersWhenInactive(t *testing.T) {
	body := renderShell(t, true)
	if !strings.Contains(body, "mx-banner") {
		t.Fatal("banner element (.mx-banner) missing when musixmatchInactive=true")
	}
	if !strings.Contains(body, `href="/settings"`) {
		t.Fatal("banner must link to /settings to add the token")
	}
	// The banner must NOT teach the operator to obtain a token by hand. This is the
	// inverse of the original #385 assertion and exists to stop the link coming back.
	if strings.Contains(body, spicetifyFAQURL) {
		t.Fatalf("banner must not link to a manual token-extraction guide (%s)", spicetifyFAQURL)
	}
	if !strings.Contains(body, "Musixmatch") {
		t.Fatal("banner copy must mention Musixmatch")
	}
	// The banner must offer the tokenless alternative (PetitLyrics), not imply all
	// lyric fetching is off (#385 follow-up: a tokenless provider may be covering).
	if !strings.Contains(body, "PetitLyrics") {
		t.Fatal("banner copy must mention the tokenless PetitLyrics alternative")
	}
}

func TestMusixmatchInactiveBannerAbsentWhenActive(t *testing.T) {
	body := renderShell(t, false)
	if strings.Contains(body, "mx-banner") {
		t.Fatal("banner element (.mx-banner) must not render when musixmatchInactive=false")
	}
	if strings.Contains(body, spicetifyFAQURL) {
		t.Fatal("Spicetify FAQ link must not render when musixmatchInactive=false")
	}
}

// TestMusixmatchCreditRendersWhenActive guards a COMPLIANCE obligation, not a
// visual preference: API Terms clause 2.1.5 requires crediting Musixmatch with a
// link to its Site each time the Data is used (docs/provider-terms.md).
//
// It is a regression guard by construction -- the credit was added and this test
// written alongside it, so it has never failed against unfixed code. Labeled as
// such rather than presented as proof the credit is correct. What it defends is
// the credit silently disappearing in a later layout edit, which is exactly how a
// compliance surface rots: nothing breaks, no test fails, and the obligation
// quietly stops being met.
func TestMusixmatchCreditRendersWhenActive(t *testing.T) {
	body := renderShell(t, false)

	if !strings.Contains(body, "mx-provider-credit") {
		t.Fatal("provider credit missing when Musixmatch is active; clause 2.1.5 requires it")
	}
	// The LINK is the required element, not the image. A credit that names
	// Musixmatch without linking to the Site does not satisfy 2.1.5.
	if !strings.Contains(body, `href="https://www.musixmatch.com"`) {
		t.Error("credit must link to the Musixmatch Site (clause 2.1.5)")
	}
	if !strings.Contains(body, "powered by Musixmatch") {
		t.Error("credit must name Musixmatch in its text")
	}
	// Rendered OUTSIDE #mx-main so an htmx report-rail swap cannot blow it away.
	// A credit that survives only until the first navigation is not an
	// each-time-you-use-the-Data credit.
	mainIdx := strings.Index(body, `id="mx-main"`)
	creditIdx := strings.Index(body, "mx-provider-credit")
	if mainIdx >= 0 && creditIdx >= 0 && creditIdx < mainIdx {
		t.Error("credit renders before #mx-main; it must sit after it, outside the htmx swap target")
	}
}

// TestMusixmatchCreditHiddenWhenInactive asserts the credit is absent with no
// usable token. Crediting Musixmatch for results another provider served would
// be a misattribution, so the obligation and its absence are both load-bearing.
func TestMusixmatchCreditHiddenWhenInactive(t *testing.T) {
	body := renderShell(t, true)
	if strings.Contains(body, "mx-provider-credit") {
		t.Error("provider credit must not render when Musixmatch is inactive; no Musixmatch Data is in use")
	}
}
