package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/providers"
)

// handleSaveSection is the multi-field ("section") settings write endpoint
// (#298). It exists because a single-field hot-save cannot bootstrap a
// cross-field invariant from an empty state: the [server.tls] rule requires
// cert_file and key_file to be set TOGETHER, so an operator with NEITHER set can
// never satisfy it one field at a time (each single POST 400s on its still-blank
// partner). This endpoint accepts several fields at once and writes them as ONE
// atomic change through config.ApplyChanges (validate-all-then-write,
// comment-preserving, temp+fsync+rename+.bak), so the pair lands together.
//
// The form carries the CSRF token, a repeated "path" naming the fields in the
// batch, and each field's value under a form key equal to its path. The order
// mirrors handleSaveField: same-origin, then CSRF, then per-field authorization
// (known, editable, not env-locked, not a secret), then the cross-field
// invariants against the RESULTING state under the write lock, then a single
// atomic write. Changes take effect on restart (there is no hot-reload).
func (u *UI) handleSaveSection(w http.ResponseWriter, r *http.Request) {
	if !enforceSameOrigin(w, r) {
		return
	}
	if !enforceCSRFToken(w, r) {
		return
	}
	if u.configPath == "" {
		http.Error(w, "settings are read-only", http.StatusForbidden)
		return
	}

	// enforceCSRFToken read a PostForm value, so the body is already parsed and
	// r.PostForm is populated; the repeated "path" lists the batch members.
	paths := r.PostForm["path"]
	if len(paths) == 0 {
		http.Error(w, "no fields to save", http.StatusBadRequest)
		return
	}

	changes := make(map[string]string, len(paths))
	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		spec, ok := config.FieldByPath(path)
		if !ok {
			http.Error(w, "unknown config field", http.StatusBadRequest)
			return
		}
		if !spec.Editable {
			http.Error(w, "field is read-only", http.StatusConflict)
			return
		}
		if fieldEnvLocked(spec) {
			http.Error(w, "field is locked by an environment override", http.StatusConflict)
			return
		}
		// Secrets (api.token, webhook keys) never travel through the section save:
		// they belong in the encrypted store, never the TOML. Reject so a forged
		// POST cannot smuggle a secret into the config file by this route.
		if spec.Sensitive {
			http.Error(w, "secret fields cannot be saved here", http.StatusBadRequest)
			return
		}
		if _, dup := changes[path]; dup {
			http.Error(w, "duplicate field in save", http.StatusBadRequest)
			return
		}
		// Each field's value is sent under a form key equal to its path. The section
		// save carries canonical values directly (it is the cert/key path pair and
		// other plain string fields); the unit/list/provider-inversion transforms of
		// the single-field path do not apply here.
		value := strings.TrimSpace(r.PostFormValue(path))
		if err := config.ValidateAndSet(path, value); err != nil {
			http.Error(w, validationMessage(err), http.StatusBadRequest)
			return
		}
		changes[path] = value
	}

	u.saveMu.Lock()
	defer u.saveMu.Unlock()
	// Cross-field invariants against the RESULTING combined state, under the write
	// lock so the read of the current config and the write are atomic. A per-field
	// check cannot see that the batch as a whole leaves a valid [server.tls] pair
	// or provider selection.
	if err := u.checkProviderInvariantChanges(r.Context(), changes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := u.checkTLSInvariantChanges(r.Context(), changes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := config.ApplyChanges(u.configPath, changes); err != nil {
		// A validation error slipping through here (state changed under us) maps to
		// 400; anything else is a write failure.
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			http.Error(w, validationMessage(err), http.StatusBadRequest)
			return
		}
		slog.Error("settings: section config write failed", "paths", paths, "error", err)
		http.Error(w, "failed to write config", http.StatusInternalServerError)
		return
	}
	writeSaveOK(w)
}

// checkTLSInvariantChanges folds the TLS-related entries of a section save onto
// the current [server.tls] state and validates the result against
// config.ValidateTLSSelection (self_signed mutually exclusive with cert/key;
// cert and key set together). It is the multi-field counterpart to the
// single-field checkTLSInvariant: it sees the whole batch at once, so a paired
// cert+key save from an empty state validates as the pair it is. Returns nil
// when no TLS field is in the batch.
func (u *UI) checkTLSInvariantChanges(ctx context.Context, changes map[string]string) error {
	cur := u.currentConfig(ctx).Server.TLS
	selfSigned, certFile, keyFile := cur.SelfSigned, cur.CertFile, cur.KeyFile
	if v, ok := changes["server.tls.self_signed"]; ok {
		selfSigned, _ = strconv.ParseBool(v) // value already type-validated upstream
	}
	if v, ok := changes["server.tls.cert_file"]; ok {
		certFile = v
	}
	if v, ok := changes["server.tls.key_file"]; ok {
		keyFile = v
	}
	return config.ValidateTLSSelection(selfSigned, certFile, keyFile)
}

// checkProviderInvariantChanges folds the provider-selection entries of a
// section save onto the current state and validates the result against
// providers.ValidateSelection (the primary must stay enabled and at least one
// provider must remain enabled). It is the multi-field counterpart to the
// single-field checkProviderInvariant. Returns nil when no provider field is in
// the batch.
func (u *UI) checkProviderInvariantChanges(ctx context.Context, changes map[string]string) error {
	cur := u.currentConfig(ctx)
	primary, disabled := cur.Providers.Primary, cur.Providers.Disabled
	if v, ok := changes["providers.primary"]; ok {
		primary = v
	}
	if v, ok := changes["providers.disabled"]; ok {
		disabled = splitCommaList(v)
	}
	return providers.ValidateSelection(primary, disabled)
}
