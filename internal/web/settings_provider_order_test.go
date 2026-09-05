package web

import (
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// TestSaveFallbackOrderPreservesSubmittedOrder pins the SERVER half of the #837
// contract: whatever order the form submits is the order persisted. This is a
// characterization test -- the write path already behaves this way, since
// formValueForField routes providers.fallback_order through the generic
// TypeStringSlice arm and joinFormSlice joins the repeated "value" fields in
// arrival order. It is worth pinning because the fix for #837 makes the CLIENT
// submit a deliberately-chosen order, and that only works if the server keeps
// it. A future refactor that sorts or canonicalizes the list here would silently
// re-break the reorder.
func TestSaveFallbackOrderPreservesSubmittedOrder(t *testing.T) {
	cases := []struct {
		name   string
		submit []string
	}{
		{"petitlyrics first", []string{"petitlyrics", "musixmatch"}},
		{"musixmatch first", []string{"musixmatch", "petitlyrics"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, cfgPath := writableTestUI(t, newFakeSecretStore())
			rec := postField(t, h, url.Values{
				"path":  {"providers.fallback_order"},
				"value": c.submit,
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if !slices.Equal(cfg.Providers.FallbackOrder, c.submit) {
				t.Errorf("fallback_order = %v, want %v (the submitted order must survive the write)",
					cfg.Providers.FallbackOrder, c.submit)
			}
		})
	}
}

// TestOrderedProviderOptionsLabelsAreBareNames drives the view-model half of
// #837. The ordered control's position IS the order, so the option label must be
// the bare provider name; the visible rank is presentation, rendered by the
// control. Baking "1. " / "2. " into the label makes the rank part of the text,
// which a reorder control would have to rewrite on every move -- and which lies
// the moment the operator drags an item without saving.
func TestOrderedProviderOptionsLabelsAreBareNames(t *testing.T) {
	opts := orderedProviderOptions([]string{"petitlyrics", "musixmatch"})

	want := []string{"petitlyrics", "musixmatch"}
	if len(opts) != len(want) {
		t.Fatalf("got %d options, want %d: %+v", len(opts), len(want), opts)
	}
	for i, w := range want {
		if opts[i].Label != w {
			t.Errorf("option %d label = %q, want the bare name %q", i, opts[i].Label, w)
		}
		if opts[i].Value != w {
			t.Errorf("option %d value = %q, want %q", i, opts[i].Value, w)
		}
		if !opts[i].Selected {
			t.Errorf("option %d (%s) is not Selected; every configured provider is in the order", i, w)
		}
	}
}

// TestOrderedProviderOptionsKeepsConfiguredOrder pins that the render order
// follows the stored order rather than the registry's declaration order. This is
// what makes a reorder visible after a save, and it is the property the fixed
// point in #837 depended on -- so it must survive the fix, not be removed with it.
func TestOrderedProviderOptionsKeepsConfiguredOrder(t *testing.T) {
	opts := orderedProviderOptions([]string{"petitlyrics", "musixmatch"})
	if len(opts) < 2 {
		t.Fatalf("got %d options, want at least 2: %+v", len(opts), opts)
	}
	if opts[0].Value != "petitlyrics" || opts[1].Value != "musixmatch" {
		t.Errorf("order = [%s %s], want [petitlyrics musixmatch] (stored order, not registry order)",
			opts[0].Value, opts[1].Value)
	}
}
