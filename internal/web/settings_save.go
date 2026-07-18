package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/secrets"
)

// handleSaveField is the single-field settings write endpoint (#288 Phase 2).
// Order is fixed: enforce same-origin, then the double-submit CSRF token, then
// resolve and authorize the field, then route the value. The Musixmatch token
// goes to the encrypted secret store (never the TOML / its .bak, #290);
// everything else is written through config.ApplyChanges, the single atomic
// (validate-all-then-write, comment-preserving, temp+fsync+rename+.bak) entry
// point. The web layer never re-implements validation: config.ValidateAndSet is
// the gate. A single-writer mutex serializes the read-modify-write so concurrent
// saves cannot interleave. Changes take effect on restart (there is no
// hot-reload); the page shows that notice.
func (u *UI) handleSaveField(w http.ResponseWriter, r *http.Request) {
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

	// Parse the form explicitly so a malformed body yields a clean 400 rather
	// than PostFormValue silently returning empty values, matching the section
	// save handler (settings_save_section.go).
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	path := strings.TrimSpace(r.PostFormValue("path"))
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
		// A field overridden by an env/CLI value is not editable here; the UI
		// disables it, so this is a defense against a forged POST.
		http.Error(w, "field is locked by an environment override", http.StatusConflict)
		return
	}

	// Secret routing: the Musixmatch token is persisted to the encrypted store,
	// never the config file. A blank submission keeps the existing value.
	if spec.Sensitive && spec.Path == "api.token" {
		u.saveSecretToken(w, r)
		return
	}

	value, err := u.formValueForField(spec, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Any other redacted/secret field (webhook keys) keeps its existing value on
	// a blank submission (leave-blank-keeps); a non-blank value is written.
	if spec.Sensitive && value == "" {
		writeSaveOK(w)
		return
	}

	if err := config.ValidateAndSet(path, value); err != nil {
		http.Error(w, validationMessage(err), http.StatusBadRequest)
		return
	}

	u.saveMu.Lock()
	defer u.saveMu.Unlock()
	// Cross-field provider invariant (under the write lock so the read of the
	// current config and the write are atomic): a single-field "is this a known
	// provider?" check can't see that the RESULTING (primary, disabled) would
	// disable the primary or every provider, which aborts boot. Reject before any
	// file mutation.
	if err := u.checkProviderInvariant(r.Context(), path, value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := u.checkTLSInvariant(r.Context(), path, value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := config.ApplyChanges(u.configPath, map[string]string{path: value}); err != nil {
		slog.Error("settings: config write failed", "path", path, "error", err)
		http.Error(w, "failed to write config", http.StatusInternalServerError)
		return
	}
	writeSaveOK(w)
}

// checkProviderInvariant validates the cross-field provider selection that would
// result from the proposed change against the loader's invariant
// (providers.ValidateSelection): the primary must not be disabled and at least
// one provider must stay enabled. Only providers.disabled and providers.primary
// change those inputs; providers.fallback_order cannot violate the invariant (a
// disabled fallback entry is simply skipped at boot), so it is not checked here.
// Returns nil for non-provider fields.
func (u *UI) checkProviderInvariant(ctx context.Context, path, value string) error {
	cur := u.currentConfig(ctx)
	primary, disabled := cur.Providers.Primary, cur.Providers.Disabled
	switch path {
	case "providers.disabled":
		disabled = splitCommaList(value)
	case "providers.primary":
		primary = value
	default:
		return nil
	}
	return providers.ValidateSelection(primary, disabled)
}

// checkTLSInvariant validates the cross-field [server.tls] selection that would
// result from the proposed change against the loader's invariant
// (config.ValidateTLSSelection): self_signed is mutually exclusive with
// cert_file/key_file, and cert_file and key_file must be set together. Only the
// three TLS fields change those inputs; the resulting-state read handles
// multi-step orderings (cert alone -> reject; cert then key -> ok; then
// self_signed -> reject). Returns nil for non-TLS fields.
func (u *UI) checkTLSInvariant(ctx context.Context, path, value string) error {
	cur := u.currentConfig(ctx).Server.TLS
	selfSigned, certFile, keyFile := cur.SelfSigned, cur.CertFile, cur.KeyFile
	switch path {
	case "server.tls.self_signed":
		selfSigned, _ = strconv.ParseBool(value) // value already type-validated upstream
	case "server.tls.cert_file":
		certFile = value
	case "server.tls.key_file":
		keyFile = value
	default:
		return nil
	}
	return config.ValidateTLSSelection(selfSigned, certFile, keyFile)
}

// splitCommaList splits a comma-joined value into trimmed, non-empty entries.
func splitCommaList(value string) []string {
	out := []string{}
	for _, p := range strings.Split(value, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// saveSecretToken persists the Musixmatch token to the encrypted secret store,
// keeping it out of the TOML and its .bak (#290). A blank value is a no-op
// success (leave-blank-keeps-existing).
func (u *UI) saveSecretToken(w http.ResponseWriter, r *http.Request) {
	if u.secretStore == nil {
		http.Error(w, "secret store unavailable", http.StatusServiceUnavailable)
		return
	}
	plaintext := r.PostFormValue("value")
	if plaintext == "" {
		writeSaveOK(w)
		return
	}
	u.saveMu.Lock()
	defer u.saveMu.Unlock()
	if err := u.secretStore.Set(r.Context(), secrets.NameMusixmatchToken, plaintext); err != nil {
		slog.Error("settings: secret store write failed", "name", secrets.NameMusixmatchToken, "error", err)
		http.Error(w, "failed to store secret", http.StatusInternalServerError)
		return
	}
	// Strip any cleartext api.token from the TOML so the encrypted store is
	// authoritative: resolveTokenWithStore ranks a file api.token ABOVE the
	// store, so a stale file value would otherwise win on next boot and mask the
	// token just saved (#288). The store write came first, so the token is never
	// lost if this step fails. No-op when the file has no api.token.
	if err := config.RemoveKeyIfPresent(u.configPath, "api.token"); err != nil {
		slog.Error("settings: failed to clear api.token from config after store save", "error", err)
		http.Error(w, "token saved to the store but clearing the config file failed; check logs", http.StatusInternalServerError)
		return
	}
	writeSaveOK(w)
}

// formValueForField derives the canonical string value to validate and store
// from the POST form, per field kind: provider enablement is inverted to the
// disabled list, durations are converted from their display unit to canonical,
// list fields are joined from repeated form values, and everything else is the
// single value field.
func (u *UI) formValueForField(spec config.FieldSpec, r *http.Request) (string, error) {
	switch {
	case spec.Path == "providers.disabled":
		// The form sends the ENABLED providers (checked boxes) as repeated
		// "value" fields; persist the inverse (the disabled list).
		return providerDisabledFromEnabled(r.PostForm["value"]), nil
	case isDurationField(spec.Path):
		return durationCanonical(spec.Path, r.PostFormValue("value"), r.PostFormValue("unit"))
	case spec.Type == config.TypeStringSlice:
		// Taglist / checkbox list: repeated "value" fields, trimmed and joined.
		return joinFormSlice(r.PostForm["value"]), nil
	default:
		return strings.TrimSpace(r.PostFormValue("value")), nil
	}
}

// fieldEnvLockSource returns the winning env var name and whether the field is
// locked by an env override. The name is the first env var in spec.EnvVars
// that is currently set and non-empty. Both the read path (Locked pill + tooltip)
// and the write path (rejecting a forged save) use fieldEnvLocked; the read
// path additionally calls this to show the source on hover (#307).
func fieldEnvLockSource(spec config.FieldSpec) (name string, locked bool) {
	for _, ev := range spec.EnvVars {
		if v, ok := os.LookupEnv(ev); ok && v != "" {
			return ev, true
		}
	}
	return "", false
}

// fieldEnvLocked reports whether an env override is currently active for the
// field. It delegates to fieldEnvLockSource and is the single bool-only signal
// shared by the write path (rejecting a forged save of a locked field).
func fieldEnvLocked(spec config.FieldSpec) bool {
	_, locked := fieldEnvLockSource(spec)
	return locked
}

// providerDisabledFromEnabled inverts the enabled-provider checkbox set into the
// stored providers.disabled list: every known provider NOT checked is disabled.
func providerDisabledFromEnabled(enabled []string) string {
	on := map[string]bool{}
	for _, e := range enabled {
		on[providers.NormalizeName(e)] = true
	}
	disabled := make([]string, 0, len(providers.Known()))
	for _, k := range providers.Known() {
		if !on[k] {
			disabled = append(disabled, k)
		}
	}
	return strings.Join(disabled, ",")
}

// isDurationField reports whether the path is a duration field edited with a
// unit selector.
func isDurationField(path string) bool {
	_, ok := durationUnits[path]
	return ok
}

// durationCanonical converts a duration entered in a display unit back to the
// field's canonical stored unit, returning an integer string (the duration
// fields are integer-valued). It rejects an unknown unit or a non-numeric value.
func durationCanonical(path, valueStr, unit string) (string, error) {
	du := durationUnits[path]
	if unit == "" {
		unit = du.display
	}
	if !slices.Contains(du.options, unit) {
		return "", fmt.Errorf("invalid unit for this field")
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(valueStr), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return "", fmt.Errorf("must be a number")
	}
	canonical := n * unitSeconds[unit] / unitSeconds[du.canonical]
	return strconv.FormatInt(int64(math.Round(canonical)), 10), nil
}

// joinFormSlice trims each repeated form value, drops blanks, and comma-joins
// the rest into the slice form config.SetValue parses (splitCSV).
func joinFormSlice(vals []string) string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}

// validationMessage extracts the human-readable reason from a config validation
// error, falling back to a generic message so an internal error never leaks.
func validationMessage(err error) string {
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		return ve.Message
	}
	return "invalid value"
}

// writeSaveOK writes the success response for a save. The body is a short plain
// confirmation; settings.js keys off the 2xx status and surfaces the
// restart-to-apply notice.
func writeSaveOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("saved"))
}
