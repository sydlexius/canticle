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
	// Musixmatch serving is the ordinary case for a banner test: inactive=false
	// means a token exists and Musixmatch is primary. The credit tests below drive
	// the two flags independently via renderShellWith, because they are NOT
	// complements (see musixmatchServing in ui.go).
	return renderShellWith(t, inactive, !inactive)
}

// renderShellWith renders the shell with both Musixmatch flags set explicitly.
// Separate from renderShell because musixmatchInactive and musixmatchServing are
// independent: a PetitLyrics-primary deployment is false for BOTH, which is
// exactly the combination that a single-flag helper cannot express and that the
// credit regressed on.
func renderShellWith(t *testing.T, inactive, serving bool) string {
	t.Helper()
	mux := http.NewServeMux()
	NewUI(config.Config{}, "v-test",
		WithMusixmatchInactive(inactive),
		WithMusixmatchServing(serving),
	).Register(mux)
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
	body := renderShellWith(t, false, true)

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
	assertOutsideMxMain(t, body, "mx-provider-credit")
}

// assertOutsideMxMain fails unless needle appears outside the #mx-main element,
// i.e. is not a descendant of the htmx swap target.
//
// It replaces a string-ORDER check (`creditIdx > mainIdx`) that was too weak, as
// both reviewers noted on PR #769: anything nested INSIDE #mx-main also appears
// after its opening tag, so the old assertion passed for exactly the arrangement
// it was meant to forbid. It also silently skipped when #mx-main was missing.
//
// Containment is computed by scanning div depth from #mx-main's opening tag to
// its matching close. That is sound here because the markup under test is
// templ-generated and therefore balanced and non-self-closing for <div>; it is
// NOT a general-purpose HTML parser. The alternative, golang.org/x/net/html, is
// currently an INDIRECT dependency, and promoting it to direct to assert one
// containment relation is a larger change to the module surface than this
// warrants.
func assertOutsideMxMain(t *testing.T, body, needle string) {
	t.Helper()

	open := strings.Index(body, `id="mx-main"`)
	if open < 0 {
		// Fail rather than skip: a renamed or removed swap target silently voids
		// this guarantee, which is the case most worth catching.
		t.Fatal(`#mx-main not found in rendered shell; the containment assertion cannot be evaluated`)
	}
	needleIdx := strings.Index(body, needle)
	if needleIdx < 0 {
		t.Fatalf("%q not found in rendered shell", needle)
	}

	// Walk from the start of #mx-main's opening tag, tracking <div> depth, and
	// stop at the index where depth returns to zero -- its matching close.
	tagStart := strings.LastIndex(body[:open], "<")
	if tagStart < 0 {
		t.Fatal("malformed markup: no opening < before #mx-main")
	}
	depth, i, closeIdx := 0, tagStart, -1
	for i < len(body) {
		switch {
		case strings.HasPrefix(body[i:], "<div"):
			depth++
			i += 4
		case strings.HasPrefix(body[i:], "</div>"):
			depth--
			i += 6
			if depth == 0 {
				closeIdx = i
			}
		default:
			i++
		}
		if closeIdx >= 0 {
			break
		}
	}
	if closeIdx < 0 {
		t.Fatal("could not locate the matching </div> for #mx-main")
	}
	if needleIdx >= tagStart && needleIdx < closeIdx {
		t.Errorf("%q renders INSIDE #mx-main (the htmx swap target); it must sit outside so a report-rail swap cannot remove it", needle)
	}
}

// TestMusixmatchCreditHiddenWhenInactive asserts the credit is absent with no
// usable token. Crediting Musixmatch for results another provider served would
// be a misattribution, so the obligation and its absence are both load-bearing.
func TestMusixmatchCreditHiddenWhenInactive(t *testing.T) {
	body := renderShellWith(t, true, false)
	if strings.Contains(body, "mx-provider-credit") {
		t.Error("provider credit must not render when Musixmatch is inactive; no Musixmatch Data is in use")
	}
}

// TestMusixmatchCreditHiddenForNonMusixmatchProvider is the regression test for
// the defect CodeRabbit caught on PR #769: the credit was gated on
// !musixmatchInactive, which is FALSE for a healthy PetitLyrics-primary
// deployment. That deployment serves no Musixmatch Data, so the credit labeled
// PetitLyrics results "Lyrics powered by Musixmatch" -- a misattribution shown to
// users, which is worse than showing no credit at all.
//
// This is the flag combination a single-boolean helper cannot express, and it is
// why the two flags are threaded separately rather than derived from each other.
//
// Unlike the other credit tests, this one DID fail against unfixed code -- it is
// a genuine regression test, not a guard. Verified before the fix landed.
func TestMusixmatchCreditHiddenForNonMusixmatchProvider(t *testing.T) {
	// The PetitLyrics-primary shape: selection succeeded (so not "inactive"), but
	// Musixmatch is not the provider serving lyrics.
	body := renderShellWith(t, false, false)

	if strings.Contains(body, "mx-provider-credit") {
		t.Error("credit must not render when another provider is serving; crediting Musixmatch for PetitLyrics results is a misattribution")
	}
	if strings.Contains(body, "powered by Musixmatch") {
		t.Error("credit text must not render when Musixmatch is not serving")
	}
	// The banner must also stay absent: this instance is healthy, just not using
	// Musixmatch. Asserted here so a future fix that conflates the two flags again
	// fails on BOTH surfaces rather than trading one wrong state for another.
	if strings.Contains(body, "mx-banner") {
		t.Error("tokenless-Musixmatch banner must not render for a healthy non-Musixmatch deployment")
	}
}
