package musixmatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCurrentClientIdentityIsAndroidNotDesktop pins #934's live migration: the
// desktop identity (host apic-desktop.musixmatch.com, app_id
// web-desktop-app-v1.0) was measured retired and must never be the active
// identity again by accident (e.g. an incomplete revert of this change). This
// asserts the LITERAL values, unlike the request-shape tests elsewhere in this
// package which only check the host and app_id agree with EACH OTHER -- that
// consistency check alone would still pass if both were reverted to desktop
// together, which is exactly the regression this test exists to catch.
func TestCurrentClientIdentityIsAndroidNotDesktop(t *testing.T) {
	const wantHost = "apic.musixmatch.com"
	const wantAppID = "android-player-v1.0"
	if currentClientIdentity.host != wantHost {
		t.Errorf("currentClientIdentity.host = %q; want %q (the retired desktop identity was apic-desktop.musixmatch.com)", currentClientIdentity.host, wantHost)
	}
	if currentClientIdentity.appID != wantAppID {
		t.Errorf("currentClientIdentity.appID = %q; want %q (the retired desktop identity was web-desktop-app-v1.0)", currentClientIdentity.appID, wantAppID)
	}
}

func TestMintToken_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("app_id"); got != currentClientIdentity.appID {
			t.Errorf("app_id = %q; want %q", got, currentClientIdentity.appID)
		}
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"tok-abcdefghijklmnopqrstuvwxyz"}}}`))
	}))
	defer srv.Close()

	m := NewTokenMinter(srv.Client())
	m.baseURL = srv.URL

	tok, err := m.Mint(context.Background())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok != "tok-abcdefghijklmnopqrstuvwxyz" {
		t.Errorf("token = %q; want the minted value", tok)
	}
}

// TestMintToken_RateLimited pins the constraint #554 measured: three rapid mints
// from one egress all return 401. That must surface as a distinct sentinel so the
// caller can back off rather than spin, which would lock itself out entirely.
func TestMintToken_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":401}}}`))
	}))
	defer srv.Close()

	m := NewTokenMinter(srv.Client())
	m.baseURL = srv.URL

	_, err := m.Mint(context.Background())
	if !errors.Is(err, ErrTokenMintRefused) {
		t.Fatalf("err = %v; want ErrTokenMintRefused", err)
	}
}

// TestMintToken_EmptyTokenIsAnError guards against persisting a useless value:
// a 200 with no user_token must fail rather than store an empty secret.
func TestMintToken_EmptyTokenIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":""}}}`))
	}))
	defer srv.Close()

	m := NewTokenMinter(srv.Client())
	m.baseURL = srv.URL

	if _, err := m.Mint(context.Background()); err == nil {
		t.Fatal("Mint: got nil error for an empty user_token; want an error")
	}
}

// TestMintToken_DegenerateTokenIsClientIdentityRetired pins #934's second
// defect: a 56-zero token (the retired desktop identity's exact shape) must be
// rejected with the DISTINCT ErrClientIdentityRetired sentinel, not merely
// "not empty so it passes". A token this shape is syntactically valid --
// right length, right character set -- and was accepted and persisted before
// this fix, which is what let the decoy-payload outage go undetected.
func TestMintToken_DegenerateTokenIsClientIdentityRetired(t *testing.T) {
	zeros := strings.Repeat("0", 56)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"message":{"header":{"status_code":200},"body":{"user_token":%q}}}`, zeros)
	}))
	defer srv.Close()

	m := NewTokenMinter(srv.Client())
	m.baseURL = srv.URL

	tok, err := m.Mint(context.Background())
	if !errors.Is(err, ErrClientIdentityRetired) {
		t.Fatalf("err = %v; want ErrClientIdentityRetired", err)
	}
	if tok != "" {
		t.Errorf("token = %q; want empty (a degenerate token must never be returned for persistence)", tok)
	}
}

// TestMintToken_SingleRepeatedCharacterIsDegenerate widens the shape beyond the
// exact all-zero case the incident measured: the issue's own AC says "empty,
// or a single repeated character", not "all zeros specifically".
func TestMintToken_SingleRepeatedCharacterIsDegenerate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`))
	}))
	defer srv.Close()

	m := NewTokenMinter(srv.Client())
	m.baseURL = srv.URL

	if _, err := m.Mint(context.Background()); !errors.Is(err, ErrClientIdentityRetired) {
		t.Fatalf("err = %v; want ErrClientIdentityRetired", err)
	}
}

// TestMintToken_EmptyVsMissingUserToken separates the two "no token" shapes: a
// user_token field PRESENT but empty is a degenerate token and must get the
// retired-identity diagnosis, while an ABSENT field is a malformed response and
// must stay a generic error rather than being misreported as a retirement.
func TestMintToken_EmptyVsMissingUserToken(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		wantRetired bool
	}{
		{"present but empty", `{"message":{"header":{"status_code":200},"body":{"user_token":""}}}`, true},
		{"field missing", `{"message":{"header":{"status_code":200},"body":{}}}`, false},
		{"wrong type", `{"message":{"header":{"status_code":200},"body":{"user_token":123}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			m := NewTokenMinter(srv.Client())
			m.baseURL = srv.URL

			tok, err := m.Mint(context.Background())
			if err == nil || tok != "" {
				t.Fatalf("Mint = (%q, %v); want an error and no token", tok, err)
			}
			if got := errors.Is(err, ErrClientIdentityRetired); got != tc.wantRetired {
				t.Fatalf("errors.Is(err, ErrClientIdentityRetired) = %v; want %v (err = %v)", got, tc.wantRetired, err)
			}
		})
	}
}

// TestMintToken_GenuineTokenIsNotFlaggedDegenerate is the false-positive guard:
// a normal, varied-character token must mint cleanly.
func TestMintToken_GenuineTokenIsNotFlaggedDegenerate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"a1b2c3d4e5f6_realistic-shape-0987"}}}`))
	}))
	defer srv.Close()

	m := NewTokenMinter(srv.Client())
	m.baseURL = srv.URL

	tok, err := m.Mint(context.Background())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok != "a1b2c3d4e5f6_realistic-shape-0987" {
		t.Errorf("token = %q; want the minted value", tok)
	}
}

// TestMintToken_RequestCarriesHostAndAppIDTogether is the #934 pinning test: a
// token is valid only for the client identity it was minted for, so the host
// and app_id must never be sent as a mismatched pair. This asserts BOTH halves
// against the same currentClientIdentity value in one request, which is what
// makes a future split (changing one without the other) fail here rather than
// ship silently.
func TestMintToken_RequestCarriesHostAndAppIDTogether(t *testing.T) {
	var gotAppID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAppID = r.URL.Query().Get("app_id")
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"tok-abcdefghijklmnopqrstuvwxyz"}}}`))
	}))
	defer srv.Close()

	m := NewTokenMinter(srv.Client())
	m.baseURL = srv.URL // stands in for currentClientIdentity.tokenURL() in production

	if _, err := m.Mint(context.Background()); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if gotAppID != currentClientIdentity.appID {
		t.Fatalf("app_id = %q; want %q (the client id currentClientIdentity.tokenURL() is minted for)", gotAppID, currentClientIdentity.appID)
	}
	// Confirm the production URL (unused by this httptest server, but the
	// value this test's baseURL override stands in for) actually embeds the
	// SAME host currentClientIdentity carries for app_id, so the pair cannot
	// drift apart: tokenURL() and appID are both read from one struct value.
	if got := currentClientIdentity.tokenURL(); !strings.Contains(got, currentClientIdentity.host) {
		t.Fatalf("tokenURL() = %q; does not contain host %q", got, currentClientIdentity.host)
	}
}

// TestNewTokenMinter_ProductionDefaultMintsAgainstCurrentIdentity pins the
// PRODUCTION default the other Mint tests override with an httptest URL: a
// minter built with no override must mint against the current identity's host.
// Without this, reverting only the mint host (leaving app_id and the lyrics
// request on the new identity) survives every test in the package, because each
// Mint test replaces baseURL before it runs.
func TestNewTokenMinter_ProductionDefaultMintsAgainstCurrentIdentity(t *testing.T) {
	m := NewTokenMinter(nil)
	const want = "https://apic.musixmatch.com/ws/1.1/token.get"
	if m.baseURL != want {
		t.Fatalf("NewTokenMinter(nil).baseURL = %q; want %q", m.baseURL, want)
	}
	if m.baseURL != currentClientIdentity.tokenURL() {
		t.Fatalf("NewTokenMinter(nil).baseURL = %q; want currentClientIdentity.tokenURL() = %q", m.baseURL, currentClientIdentity.tokenURL())
	}
}

// TestMintToken_CaptchaResponseIsRateLimited pins the specific #934 shape: a
// quick second token.get from one IP answers with an inner status_code 401
// AND a captcha challenge embedded in the body, still wrapped in an outer
// HTTP 200. This must map to the SAME ErrTokenMintRefused sentinel as the
// plain 401 case (TestMintToken_RateLimited) -- the captcha detail is
// incidental to the classification, which keys on status_code alone, not on
// any particular body shape naming it. Existing persistence (a refused mint
// leaves the previously-stored token untouched, see
// TestBootstrapToken_RefusedMintDegrades in internal/commands) already
// prevents this from causing re-mint churn.
func TestMintToken_CaptchaResponseIsRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":401,"hint":"captcha"},"body":{"captcha":"required"}}}`))
	}))
	defer srv.Close()

	m := NewTokenMinter(srv.Client())
	m.baseURL = srv.URL

	_, err := m.Mint(context.Background())
	if !errors.Is(err, ErrTokenMintRefused) {
		t.Fatalf("err = %v; want ErrTokenMintRefused", err)
	}
}

func TestMintToken_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	m := NewTokenMinter(srv.Client())
	m.baseURL = srv.URL

	if _, err := m.Mint(context.Background()); err == nil {
		t.Fatal("Mint: got nil error for a malformed body; want an error")
	}
}

func TestMintToken_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := NewTokenMinter(srv.Client())
	m.baseURL = srv.URL

	if _, err := m.Mint(context.Background()); err == nil {
		t.Fatal("Mint: got nil error for HTTP 500; want an error")
	}
}
