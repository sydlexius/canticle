package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/providers"
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

	// Parse the form explicitly so r.PostForm is populated by this handler rather
	// than relying on enforceCSRFToken's parse-as-a-side-effect; the repeated
	// "path" then lists the batch members.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	paths := r.PostForm["path"]
	if len(paths) == 0 {
		http.Error(w, "no fields to save", http.StatusBadRequest)
		return
	}

	// Batch semantics (#836): validate EVERY member and collect per-field errors
	// instead of stopping at the first, then run the cross-field checkers on the
	// surviving subset, then do ONE atomic write or reject the whole batch. A
	// batch with any error writes nothing, so the file never holds a partial save.
	errs := sectionErrors{}
	changes := make(map[string]string, len(paths))
	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		if status, msg := authorizeSectionField(path); msg != "" {
			errs.add(path, status, msg)
			continue
		}
		if _, dup := changes[path]; dup {
			errs.add(path, http.StatusBadRequest, "duplicate field in save")
			continue
		}
		// Each field's value is sent under a form key equal to its path. The section
		// save carries canonical values directly (it is the cert/key path pair and
		// other plain string fields); the unit/list/provider-inversion transforms of
		// the single-field path do not apply here.
		value := strings.TrimSpace(r.PostFormValue(path))
		if err := config.ValidateAndSet(path, value); err != nil {
			errs.add(path, http.StatusBadRequest, validationMessage(err))
			continue
		}
		changes[path] = value
	}

	u.saveMu.Lock()
	defer u.saveMu.Unlock()
	// Cross-field invariants against the RESULTING combined state, under the write
	// lock so the read of the current config and the write are atomic. A per-field
	// check cannot see that the batch as a whole leaves a valid [server.tls] pair
	// or provider selection. A group is judged only when every submitted member of
	// it survived field validation: a half-rejected pair would otherwise add a
	// misleading "set together" error on top of the real one.
	for _, g := range u.crossFieldGroups() {
		if !errs.groupJudgeable(g.paths, changes) {
			continue
		}
		if err := g.check(r.Context(), changes); err != nil {
			for _, p := range g.paths {
				if _, ok := changes[p]; ok {
					errs.add(p, http.StatusBadRequest, err.Error())
				}
			}
		}
	}
	if errs.any() {
		errs.write(w, r)
		return
	}
	if err := config.ApplyChanges(u.configPath, changes); err != nil {
		// A validation error slipping through here (state changed under us) maps to
		// 400; anything else is a write failure.
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			errs.add(ve.Path, http.StatusBadRequest, validationMessage(err))
			errs.write(w, r)
			return
		}
		slog.Error("settings: section config write failed", "count", len(paths), "error", err)
		http.Error(w, "failed to write config", http.StatusInternalServerError)
		return
	}
	written := make([]string, 0, len(changes))
	for p := range changes {
		written = append(written, p)
	}
	writeSaveOK(w, written...)
}

// authorizeSectionField applies the per-field gates of the section save: known,
// editable, not env-locked, not a secret. It returns the HTTP status and message
// of the first gate that fails, or an empty message when the field may be saved.
func authorizeSectionField(path string) (int, string) {
	spec, ok := config.FieldByPath(path)
	switch {
	case !ok:
		return http.StatusBadRequest, "unknown config field"
	case !spec.Editable:
		return http.StatusConflict, "field is read-only"
	case fieldEnvLocked(spec):
		return http.StatusConflict, "field is locked by an environment override"
	case spec.Sensitive:
		// Secrets (api.token, webhook keys) never travel through the section save:
		// they belong in the encrypted store, never the TOML. Reject so a forged
		// POST cannot smuggle a secret into the config file by this route.
		return http.StatusBadRequest, "secret fields cannot be saved here"
	}
	return 0, ""
}

// crossFieldGroup is one cross-field invariant: the paths it reads and the
// checker that validates the resulting combined state.
type crossFieldGroup struct {
	paths []string
	check func(ctx context.Context, changes map[string]string) error
}

// crossFieldGroups lists every cross-field invariant the section save enforces.
func (u *UI) crossFieldGroups() []crossFieldGroup {
	return []crossFieldGroup{
		{[]string{"providers.primary", "providers.disabled"}, u.checkProviderInvariantChanges},
		{[]string{"server.tls.self_signed", "server.tls.cert_file", "server.tls.key_file"}, u.checkTLSInvariantChanges},
		{[]string{"server.scan_schedule.frequency", "server.scan_schedule.at", "server.scan_schedule.day"}, u.checkScanScheduleInvariantChanges},
		{[]string{"instrumental_detector.ordering", "providers.mode"}, u.checkDetectorOrderingInvariantChanges},
	}
}

// sectionErrors accumulates per-field errors for one batch, keyed by path, and
// remembers the HTTP status to answer with (409 if any error is a 409, else the first error's).
type sectionErrors struct {
	byPath map[string]string
	order  []string
	status int
}

func (e *sectionErrors) add(path string, status int, msg string) {
	if e.byPath == nil {
		e.byPath = map[string]string{}
	}
	if _, seen := e.byPath[path]; seen {
		return
	}
	e.byPath[path] = msg
	e.order = append(e.order, path)
	// 409 (read-only / env-locked) wins over 400 so the batch status does not
	// depend on field order.
	if e.status == 0 || (status == http.StatusConflict && e.status != http.StatusConflict) {
		e.status = status
	}
}

func (e *sectionErrors) any() bool { return len(e.byPath) > 0 }

// groupJudgeable reports whether a cross-field group should be checked: at least
// one member is in the surviving subset and no submitted member of it failed.
func (e *sectionErrors) groupJudgeable(group []string, changes map[string]string) bool {
	present := false
	for _, p := range group {
		if _, ok := changes[p]; ok {
			present = true
		}
		if _, failed := e.byPath[p]; failed {
			return false
		}
	}
	return present
}

// sectionErrorResponse is the JSON error body for a rejected batch. Errors maps
// each failing path to its message so a client can mark the matching field;
// Error is a one-line summary for display where no per-field mapping exists.
type sectionErrorResponse struct {
	Status string            `json:"status"`
	Error  string            `json:"error"`
	Errors map[string]string `json:"errors"`
}

// write answers a rejected batch. A client that asks for JSON (Accept:
// application/json) gets the per-field map; any other client gets the plain-text
// summary today's settings.js already renders, so this is not a breaking change.
func (e *sectionErrors) write(w http.ResponseWriter, r *http.Request) {
	summary := e.byPath[e.order[0]]
	if len(e.order) > 1 {
		// A cross-field conflict repeats one message across its members; list each
		// distinct message once. The JSON map keeps an entry per path.
		parts := make([]string, 0, len(e.order))
		seen := map[string]bool{}
		for _, p := range e.order {
			m := e.byPath[p]
			if seen[m] {
				continue
			}
			seen[m] = true
			parts = append(parts, p+": "+m)
		}
		summary = strings.Join(parts, "; ")
	}
	if !strings.Contains(r.Header.Get("Accept"), "application/json") {
		http.Error(w, summary, e.status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(e.status)
	if err := json.NewEncoder(w).Encode(sectionErrorResponse{Status: "error", Error: summary, Errors: e.byPath}); err != nil {
		slog.Error("settings: write section error response failed", "error", err)
	}
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

// checkScanScheduleInvariantChanges folds the [server.scan_schedule] entries of
// a section save onto the current state and validates the result against
// config.ValidateScanSchedule ("daily"/"weekly" require "at"; "weekly" also
// requires "day"). It is the multi-field counterpart to the single-field
// checkScanScheduleInvariant, needed for the same reason the TLS pair needs one:
// a batch save touching frequency, at, and day at once must be validated as the
// combination it produces, not as three independently-valid fields. Returns nil
// when no schedule field is in the batch.
func (u *UI) checkScanScheduleInvariantChanges(ctx context.Context, changes map[string]string) error {
	cur := u.currentConfig(ctx).Server.ScanSchedule
	if v, ok := changes["server.scan_schedule.frequency"]; ok {
		cur.Frequency = v
	}
	if v, ok := changes["server.scan_schedule.at"]; ok {
		cur.At = v
	}
	if v, ok := changes["server.scan_schedule.day"]; ok {
		cur.Day = v
	}
	var cfg config.Config
	cfg.Server.ScanSchedule = cur
	return config.ValidateScanSchedule(cfg)
}

// checkDetectorOrderingInvariantChanges folds the ordering/mode entries of a
// batch save onto the current state and validates the result against
// config.ValidateInstrumentalDetectorOrdering ("front" requires ordered
// dispatch). It is the multi-field counterpart to the single-field
// checkDetectorOrderingInvariant.
//
// The batch lane needs its own check for the reason the TLS pair does: judging
// either field alone against the CURRENT config gets BOTH answers wrong. A batch
// that switches to parallel WHILE demoting the ordering is legal and must be
// accepted; a batch that sets both halves of the conflict at once is fatal and
// must be refused, even though each value is individually valid. Only the
// resulting combination distinguishes them. Returns nil when neither field is in
// the batch.
func (u *UI) checkDetectorOrderingInvariantChanges(ctx context.Context, changes map[string]string) error {
	ordering, hasOrdering := changes["instrumental_detector.ordering"]
	mode, hasMode := changes["providers.mode"]
	if !hasOrdering && !hasMode {
		return nil
	}
	// Read the config only once a relevant field is actually in the batch:
	// currentConfig re-reads and re-parses the file on every call, so validating
	// an untouched pair would cost a file read to re-confirm what the last boot
	// already validated.
	cur := u.currentConfig(ctx)
	if hasOrdering {
		cur.InstrumentalDetector.Ordering = ordering
	}
	if hasMode {
		cur.Providers.Mode = mode
	}
	return config.ValidateInstrumentalDetectorOrdering(cur)
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
