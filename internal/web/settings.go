package web

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/sydlexius/mxlrcgo-svc/internal/auth"
	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/providers"
	"github.com/sydlexius/mxlrcgo-svc/web/templates"
)

// handleSettings renders the editable-settings page read path (#288 Phase 1). It
// builds the view from the field registry and the effective config: each field
// carries its current value (secrets redacted), lock status (an env override in
// effect), the locking env var, and its criticality tier. No write happens on
// this path; the write handlers arrive in Phase 2.
//
// The view exposes operational detail (paths, intervals, provider lanes) even
// with secrets redacted, so the response is marked no-store to keep it out of
// browser and intermediary caches, matching the Config and Reports pages.
func (u *UI) handleSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Render from the CURRENT on-disk config (re-loaded + env-resolved per
	// request), not the frozen startup snapshot, so a just-saved value shows on
	// reload (#288 Phase 2). The "takes effect on restart" notice still applies to
	// runtime behavior; the displayed value reflects what is on disk now.
	view := u.buildSettingsView(u.currentConfig(r.Context()))

	// The write path is enabled only when a config file path is wired. Embed a
	// double-submit CSRF token for the save POSTs; if the page is read-only or
	// token generation fails, leave Writable false so no save controls render.
	if u.configPath != "" {
		if token, err := ensureCSRFToken(w, r, u.secureRequest(r)); err == nil {
			view.CSRFToken = token
			view.Writable = true
		} else {
			slog.Error("settings: CSRF token generation failed; page stays read-only", "error", err)
		}
	}
	// Mark which fields can actually be saved (write path wired, editable, not
	// env-locked) so the template renders save controls only where a save would
	// be accepted.
	if view.Writable {
		markSavable(view.Common)
		for i := range view.Sections {
			markSavable(view.Sections[i].Fields)
		}
	}
	render(w, r, templates.SettingsPage(u.version, view, u.buildRail("")))
}

// markSavable flags each field the page may write: editable and not locked by
// an env override. Called only when the write path is wired (view.Writable), so
// a field is Savable exactly when a POST for it would be accepted.
func markSavable(fields []templates.SettingsField) {
	for i := range fields {
		fields[i].Savable = fields[i].Editable && !fields[i].Locked
	}
}

// settingsSectionOrder is the section display order and human titles. It is the
// locked design order; every registry section MUST appear here (a missing
// section would silently drop its fields, caught by TestSettingsViewCoversRegistry).
var settingsSectionOrder = []struct {
	key   string
	title string
}{
	{"api", "API"},
	{"output", "Output"},
	{"db", "Database"},
	{"server", "Server"},
	{"providers", "Providers"},
	{"verification", "Verification"},
	{"instrumental_detector", "Instrumental Detector"},
	{"enrichment", "Enrichment"},
	{"guard", "Guard"},
	{"queue", "Queue"},
	{"logging", "Logging"},
	{"secrets", "Secrets"},
}

// commonPaths is the everyday field set shown on the default Common tab, in the
// exact display order from the #288 UAT. Every other registry field falls to the
// Advanced tab. This is also the dedup key: a path here is never rendered in
// Advanced.
var commonPaths = []string{
	"api.token",
	"api.cooldown",
	"output.embedded_lyrics",
	"output.bilingual_output",
	"providers.disabled",
	"providers.primary",
	"providers.mode",
	"server.addr",
	"server.webhook_api_keys",
	"server.scan_interval_seconds",
	"enrichment.enabled",
	"logging.level",
}

// uiHiddenPaths are registry fields intentionally NOT rendered as editable
// controls on the settings page. The key, its validation, env var, and TOML
// entry all remain (power users edit it via TOML/env), and it still shows in the
// read-only Raw config tab; it is only suppressed from the editable Common and
// Advanced tabs. output.dir is hidden pending a backend decision on its UX
// (#288 UAT).
var uiHiddenPaths = map[string]bool{
	"output.dir": true,
	// Both ffmpeg path fields are dropped from the editable tabs (#288 E8, same
	// C9 pattern as output.dir): the real fix is auto-provisioning ffmpeg (#293),
	// which removes the path config surface entirely, so a UI for them now would
	// be throwaway. Registry entries (validation, env vars, TOML, drift test)
	// stay intact and the values still show in the read-only Raw config tab.
	"verification.ffmpeg_path":          true,
	"instrumental_detector.ffmpeg_path": true,
}

// buildSettingsView maps the field registry plus the effective config onto the
// Settings page view model: the Common tab (commonPaths, in order), the Advanced
// tab (every other field grouped by section), and the Raw config tab (the
// effective config rendered as redacted TOML). Fields in uiHiddenPaths are
// omitted from both editable tabs but remain in the Raw config tab.
func (u *UI) buildSettingsView(cfg config.Config) templates.SettingsView {
	common := map[string]bool{}
	for _, p := range commonPaths {
		common[p] = true
	}

	view := templates.SettingsView{
		// FormatConfigText is the single redaction source of truth (shared with
		// the logging layer); secrets are masked before the text reaches the
		// template. Source-hint maps are nil: the Raw tab shows merged effective
		// values, not per-field provenance.
		RawTOML: annotateRawConfig(config.FormatConfigText(cfg, nil, nil)),
	}

	// Common tab: build in commonPaths order. A path missing from the registry
	// would be a programming error (caught by TestSettingsCommonPathsValid).
	for _, p := range commonPaths {
		if spec, ok := config.FieldByPath(p); ok {
			view.Common = append(view.Common, u.settingsField(cfg, spec))
		}
	}

	// Advanced tab: every non-common, non-hidden field, grouped by section.
	bySection := map[string][]templates.SettingsField{}
	for _, spec := range config.Registry() {
		if common[spec.Path] || uiHiddenPaths[spec.Path] {
			continue
		}
		bySection[spec.Section] = append(bySection[spec.Section], u.settingsField(cfg, spec))
	}
	for _, s := range settingsSectionOrder {
		fields := bySection[s.key]
		if len(fields) == 0 {
			continue
		}
		view.Sections = append(view.Sections, templates.SettingsSection{
			Key:    s.key,
			Title:  s.title,
			Fields: fields,
		})
	}
	return view
}

// settingsField builds one field's view model from its registry spec and the
// effective config. Lock status is derived from the env (an override present and
// non-empty); the effective value is read from the merged config and redacted
// for secrets. The control kind (InputType) plus its Options / duration units /
// generate-key prefix are derived so the template renders a guided control (no
// free-text box for a fixed-choice value).
func (u *UI) settingsField(cfg config.Config, spec config.FieldSpec) templates.SettingsField {
	f := templates.SettingsField{
		Path:        spec.Path,
		DOMID:       settingsDOMID(spec.Path),
		Label:       settingsLabel(spec),
		Description: settingsDescriptions[spec.Path],
		Sensitive:   spec.Sensitive,
		Editable:    spec.Editable,
		Tier:        criticalityTier(spec.Criticality),
	}

	// Lock status: the field is locked when one of its own env vars is set
	// non-empty (an active override). CLI overrides are not visible to a
	// long-running daemon's process env, so the env presence check is the
	// read-path signal. The card shows only a plain "Locked" pill, never the
	// variable name. fieldEnvLocked is shared with the write path so a locked
	// field is rejected on save, not just disabled in the UI.
	f.Locked = fieldEnvLocked(spec)

	f.EffectiveValue = u.effectiveValue(cfg, spec)
	f.InputType = settingsInputType(spec)
	f.EnableWhenChecked = enableController(spec.Path)
	f.Placeholder = fieldPlaceholders[spec.Path]

	switch f.InputType {
	case "bool":
		f.Options = boolOptions(spec.Path, f.EffectiveValue)
	case "select":
		f.Options = selectOptions(spec.Path, f.EffectiveValue)
	case "providers":
		// Provider enablement: a checkbox per known provider, checked = enabled
		// (the inverse of the stored providers.disabled list).
		f.Options = providerEnableOptions(cfg.Providers.Disabled, cfg.Providers.Primary)
	case "ordered":
		f.Options = orderedProviderOptions(cfg.Providers.FallbackOrder)
	case "duration":
		f.DisplayValue, f.DisplayUnit, f.UnitOptions = durationDisplay(spec.Path, cfg)
	case "taglist":
		f.ListValues = configSliceValue(cfg, spec.Path)
		f.Placeholder = taglistPlaceholders[spec.Path]
	case "webhook":
		f.GenPrefix = auth.KeyPrefix
	}

	switch spec.Path {
	case "providers.mode":
		// E5: a jump link to the Advanced "order to try sources" section, since
		// the mode (ordered/parallel) determines how that order is used. settings.js
		// only surfaces it when "in order" is selected (#288 G3).
		f.JumpTargetID = settingsDOMID("providers.fallback_order")
		f.JumpTab = "mx-tab-advanced"
		f.JumpLabel = "Set the order to try sources"
	case "server.tls.redirect_http":
		// G4: placeholder derived from this install's server.addr host.
		f.Placeholder = redirectPlaceholder(cfg.Server.Addr)
	case "server.tls.cert_file", "server.tls.key_file":
		// #298: the [server.tls] cert+key invariant requires both set together, so
		// the two cards share a save group; settings.js routes their Save to the
		// atomic /settings/section endpoint, letting an operator bootstrap a custom
		// cert pair from an empty state (a single-field save always 400s on the
		// still-blank partner).
		f.SaveGroup = tlsCertKeySaveGroup
	}
	return f
}

// tlsCertKeySaveGroup is the save-group token shared by the server.tls.cert_file
// and server.tls.key_file cards so settings.js posts them together to
// /settings/section as one atomic change (#298). The pair must be written
// together to satisfy the [server.tls] "cert and key set together" invariant.
const tlsCertKeySaveGroup = "tls-cert-key"

// criticalityTier maps a registry Criticality to the save-trigger tier string
// the template and settings.js branch on: safe hot-saves on change, caution
// needs an explicit Save button, critical needs Save plus a confirm dialog.
func criticalityTier(c config.Criticality) string {
	switch c {
	case config.Critical:
		return "critical"
	case config.Caution:
		return "caution"
	default:
		return "safe"
	}
}

// settingsDOMID is the unique element id for a field's primary control: the
// dotted path with dots replaced by dashes (e.g. "field-api-token"). It is the
// single id-derivation rule shared by the field builder and the gating wiring,
// so a dependent field can reference its controller's id without guessing.
func settingsDOMID(path string) string {
	return "field-" + strings.ReplaceAll(path, ".", "-")
}

// enableController returns the DOM id of the control whose checked state enables
// (un-greys) the given field, or "" if the field is not gated. The gating is
// purely client-side (settings.js): a child input is disabled until its
// controller checkbox/radio is checked. The credential field is gated by its
// provider's enablement checkbox; the verification/detector child fields are
// gated by their section's "enabled" radio.
func enableController(path string) string {
	if path == "api.token" {
		return providerEnableCheckboxID(providers.Musixmatch)
	}
	return fieldEnabledBy[path]
}

// fieldEnabledBy maps a child field to the DOM id of the "enabled" control that
// must be checked for it to be editable. Verification and instrumental-detector
// child fields grey out when their section is switched off (#288 D7/D8).
var fieldEnabledBy = buildFieldEnabledBy()

func buildFieldEnabledBy() map[string]string {
	m := map[string]string{}
	for _, p := range []string{
		"verification.whisper_url",
		"verification.sample_duration_seconds",
		"verification.min_confidence",
		"verification.min_similarity",
	} {
		m[p] = settingsDOMID("verification.enabled")
	}
	// Neither ffmpeg path field is listed here: both are hidden from the editable
	// tabs (#288 E8 / uiHiddenPaths), so they render no gated control.
	for _, p := range []string{
		"instrumental_detector.classifier_url",
		"instrumental_detector.sample_duration_seconds",
		"instrumental_detector.min_confidence",
		"instrumental_detector.instrumental_classes",
		"instrumental_detector.cooldown_seconds",
	} {
		m[p] = settingsDOMID("instrumental_detector.enabled")
	}
	// TLS: cert_file / key_file are usable only when self_signed is OFF (they are
	// mutually exclusive, #288). boolOptions renders the false radio at index 1,
	// so OptionID(domid, 1) == domid+"-1" is its id; the field is enabled while
	// that radio is checked. The reverse (disable self_signed when cert/key are
	// set) is the syncTLS handler in settings.js; the hard net is checkTLSInvariant.
	offRadio := settingsDOMID("server.tls.self_signed") + "-1"
	m["server.tls.cert_file"] = offRadio
	m["server.tls.key_file"] = offRadio
	return m
}

// fieldPlaceholders supplies an example value as the input placeholder for
// free-text fields where the expected format is not obvious (#288 E4/E7b).
var fieldPlaceholders = map[string]string{
	"logging.file": "/config/mxlrcgo.log",
}

// redirectPlaceholder derives the HTTP->HTTPS redirect placeholder from this
// install's configured server.addr host on the standard HTTP port, so the hint
// references the operator's own value rather than a fixed example (#288 G4).
func redirectPlaceholder(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return "127.0.0.1:80"
	}
	return net.JoinHostPort(host, "80")
}

// taglistPlaceholders marks the address-list fields rendered as an add/remove
// list control (#288 D4) and supplies each one's input placeholder. A list
// entry is individually removable rather than buried in a comma-joined box.
var taglistPlaceholders = map[string]string{
	"server.trusted_networks.cidrs":           "192.168.1.0/24",
	"server.trusted_networks.trusted_proxies": "192.168.1.0/24",
	"server.tls.self_signed_hosts":            "hostname or IP",
	"guard.accepted_scripts":                  "Latin, Han, Kana",
}

// settingsInputType picks the guided control for a field: explicit per-path
// kinds first (secrets, webhook, provider pickers, duration), then closed enums
// (dropdown), then booleans (radio pair), then the plain type-based input.
func settingsInputType(spec config.FieldSpec) string {
	switch spec.Path {
	case "api.token":
		return "secret"
	case "server.webhook_api_keys":
		return "webhook"
	case "providers.primary":
		return "select"
	case "providers.disabled":
		return "providers"
	case "providers.fallback_order":
		return "ordered"
	}
	if _, ok := taglistPlaceholders[spec.Path]; ok {
		return "taglist"
	}
	if _, ok := durationUnits[spec.Path]; ok {
		return "duration"
	}
	if config.AllowedValues(spec.Path) != nil {
		return "select"
	}
	if spec.Type == config.TypeBool {
		return "bool"
	}
	return inputType(spec.Type)
}

// boolLabels gives each boolean field a meaningful label for its two radio
// choices (on-label, off-label), so the page never renders a bare true/false.
var boolLabels = map[string][2]string{
	"output.bilingual_output":       {"Save original and translation together", "Save one language only"},
	"verification.enabled":          {"Verify lyrics against the audio", "Don't verify"},
	"instrumental_detector.enabled": {"Detect instrumental tracks", "Don't detect"},
	"enrichment.enabled":            {"Look up extra track info first", "Skip the lookup"},
	"queue.randomize":               {"Process in random order", "Process in order"},
	"server.tls.self_signed":        {"Use a self-signed certificate", "Off"},
	"logging.compress":              {"Compress old log files", "Don't compress"},
}

// boolOptions builds the two labeled radio choices for a boolean field, marking
// the one matching the current value selected.
func boolOptions(path, effective string) []templates.SettingsOption {
	tl, fl := "On", "Off"
	if lbl, ok := boolLabels[path]; ok {
		tl, fl = lbl[0], lbl[1]
	}
	isTrue := effective == "true"
	return []templates.SettingsOption{
		{Value: "true", Label: tl, Selected: isTrue},
		{Value: "false", Label: fl, Selected: !isTrue},
	}
}

// modeOptionLabels gives providers.mode's two enum values plain-language labels
// so the dropdown reads as a choice, not a jargon token, and so it does not
// collide with the separate "which source to try first" order control (#288 G3).
var modeOptionLabels = map[string]string{
	"ordered":  "In order (try one, then the next)",
	"parallel": "In parallel (race them)",
}

// selectOptions builds the dropdown choices for a fixed-choice field: the
// provider list for providers.primary, otherwise the validation enum set.
func selectOptions(path, effective string) []templates.SettingsOption {
	var vals []string
	if path == "providers.primary" {
		vals = providers.Known()
	} else {
		vals = config.AllowedValues(path)
	}
	opts := make([]templates.SettingsOption, 0, len(vals))
	for _, v := range vals {
		label := v
		if path == "providers.mode" {
			if l, ok := modeOptionLabels[v]; ok {
				label = l
			}
		}
		opts = append(opts, templates.SettingsOption{Value: v, Label: label, Selected: v == effective})
	}
	return opts
}

// providerEnableOptions builds a checkbox option per known provider, where
// checked means "enabled" (the provider is NOT in the disabled list). It is the
// inverse view of providers.disabled, so the operator picks the sources to use
// rather than the sources to exclude (#288 D1). Either or both may be enabled.
func providerEnableOptions(disabled []string, primary string) []templates.SettingsOption {
	dis := map[string]bool{}
	for _, d := range disabled {
		dis[providers.NormalizeName(d)] = true
	}
	prim := providers.NormalizeName(primary)
	if prim == "" {
		prim = providers.Musixmatch
	}
	opts := make([]templates.SettingsOption, 0, len(providers.Known()))
	for _, k := range providers.Known() {
		opt := templates.SettingsOption{Value: k, Label: k, Selected: !dis[k]}
		if k == prim {
			// The primary source can't be disabled here (it would abort boot); show
			// it enabled and non-toggleable. The cross-field save validator is the
			// hard safety net regardless (#288).
			opt.Selected = true
			opt.Fixed = true
			opt.Label = k + " (primary)"
		}
		opts = append(opts, opt)
	}
	return opts
}

// providerEnableCheckboxID returns the DOM id of a provider's enablement
// checkbox. The provider-enable control renders its checkboxes with
// templates.OptionID(<providers.disabled DOMID>, i), so the id is derived the
// same way here, letting a gated credential field reference its provider's
// checkbox by id regardless of the provider's position in the known list.
func providerEnableCheckboxID(name string) string {
	base := settingsDOMID("providers.disabled")
	for i, k := range providers.Known() {
		if k == providers.NormalizeName(name) {
			return templates.OptionID(base, i)
		}
	}
	return base
}

// configSliceValue returns the raw string slice for an address-list field
// (rendered as the add/remove taglist control), so each entry can be shown as
// an individually-removable item rather than a comma-joined string.
func configSliceValue(cfg config.Config, path string) []string {
	switch path {
	case "server.trusted_networks.cidrs":
		return cfg.Server.TrustedNetworks.Cidrs
	case "server.trusted_networks.trusted_proxies":
		return cfg.Server.TrustedNetworks.TrustedProxies
	case "server.tls.self_signed_hosts":
		return cfg.Server.TLS.SelfSignedHosts
	case "guard.accepted_scripts":
		return cfg.Guard.AcceptedScripts
	}
	return nil
}

// orderedProviderOptions renders the fallback order as an ordered pick list: the
// configured providers first (in order, numbered), then the remaining known
// providers unselected. Reordering is a Phase 2 control; this read path shows
// the current order without a free-text box.
func orderedProviderOptions(order []string) []templates.SettingsOption {
	pos := map[string]int{}
	opts := []templates.SettingsOption{}
	n := 0
	for _, p := range order {
		k := providers.NormalizeName(p)
		if !providers.IsKnown(k) {
			continue
		}
		if _, dup := pos[k]; dup {
			continue
		}
		n++
		pos[k] = n
		opts = append(opts, templates.SettingsOption{Value: k, Label: strconv.Itoa(n) + ". " + k, Selected: true})
	}
	for _, k := range providers.Known() {
		if _, ok := pos[k]; ok {
			continue
		}
		opts = append(opts, templates.SettingsOption{Value: k, Label: k})
	}
	return opts
}

// durationUnit describes how a time-valued field is stored (canonical) and the
// natural unit it is shown in, plus the units offered in the dropdown.
type durationUnit struct {
	canonical string
	display   string
	options   []string
}

// durationUnits maps each time-valued field to its display units. The canonical
// unit matches how the value is stored; the conversion on SAVE is Phase 2.
var durationUnits = map[string]durationUnit{
	"api.cooldown":                     {"seconds", "seconds", []string{"seconds", "minutes", "hours"}},
	"api.circuit_open_duration":        {"seconds", "minutes", []string{"seconds", "minutes", "hours"}},
	"api.circuit_backoff_base_seconds": {"seconds", "seconds", []string{"seconds", "minutes", "hours"}},
	// Defaults exceed 24h (168h = 7 days, 672h = 28 days), so they display in
	// days by default while still stored canonically in hours (#288 D3).
	"api.miss_backoff_base_hours":                   {"hours", "days", []string{"hours", "days"}},
	"api.miss_backoff_cap_hours":                    {"hours", "days", []string{"hours", "days"}},
	"server.scan_interval_seconds":                  {"seconds", "minutes", []string{"seconds", "minutes", "hours"}},
	"server.work_interval_seconds":                  {"seconds", "minutes", []string{"seconds", "minutes", "hours"}},
	"verification.sample_duration_seconds":          {"seconds", "seconds", []string{"seconds", "minutes"}},
	"instrumental_detector.sample_duration_seconds": {"seconds", "seconds", []string{"seconds", "minutes"}},
	"instrumental_detector.cooldown_seconds":        {"seconds", "seconds", []string{"seconds", "minutes"}},
	"providers.race_wait_seconds":                   {"seconds", "seconds", []string{"seconds", "minutes"}},
	"logging.max_age_days":                          {"days", "days", []string{"hours", "days"}},
}

// unitSeconds is the number of seconds in each duration unit.
var unitSeconds = map[string]float64{"seconds": 1, "minutes": 60, "hours": 3600, "days": 86400}

// durationDisplay converts a field's canonical value into its natural display
// unit, returning the display number, the display unit, and the unit options.
func durationDisplay(path string, cfg config.Config) (value, unit string, options []string) {
	du := durationUnits[path]
	raw := rawConfigValue(cfg, path)
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return raw, du.display, du.options
	}
	canonicalSecs := n * unitSeconds[du.canonical]
	disp := canonicalSecs / unitSeconds[du.display]
	return strconv.FormatFloat(disp, 'g', -1, 64), du.display, du.options
}

// annotateRawConfig prepends a "# friendly name" comment above each key line in
// the rendered TOML, sourced from the same settingsLabels used for the field
// labels (single source). Display-only: writing these comments into the real
// config file on save is tracked separately (#291).
func annotateRawConfig(raw string) string {
	var b strings.Builder
	section := ""
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		if eq := strings.Index(line, " = "); eq > 0 && section != "" && !strings.HasPrefix(trimmed, "#") {
			path := section + "." + strings.TrimSpace(line[:eq])
			if label, ok := settingsLabels[path]; ok {
				b.WriteString("# ")
				b.WriteString(label)
				b.WriteByte('\n')
			}
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// effectiveValue renders the field's current merged value as a string. Secret
// fields never echo their stored value: the token shows a set/not-set state and
// the webhook keys show a count, so an operator can tell a secret exists without
// revealing it.
func (u *UI) effectiveValue(cfg config.Config, spec config.FieldSpec) string {
	if spec.Sensitive {
		switch spec.Path {
		case "api.token":
			if cfg.API.Token != "" {
				return "(set)"
			}
			return "(not set)"
		case "server.webhook_api_keys":
			n := len(cfg.Server.WebhookAPIKeys)
			if n == 0 {
				return "(none)"
			}
			if n == 1 {
				return "1 key configured"
			}
			return strconv.Itoa(n) + " keys configured"
		default:
			return "(redacted)"
		}
	}
	return rawConfigValue(cfg, spec.Path)
}

// rawConfigValue returns the non-sensitive effective value for a dotted path as
// a display string. Slices render comma-joined; an empty slice renders as "[]".
// A path with no case here returns the empty string (it must be a sensitive
// field handled by effectiveValue, or a registry/Config drift caught by tests).
func rawConfigValue(cfg config.Config, path string) string {
	switch path {
	// [api]
	case "api.cooldown":
		return strconv.Itoa(cfg.API.Cooldown)
	case "api.circuit_open_duration":
		return strconv.Itoa(cfg.API.CircuitOpenDuration)
	case "api.circuit_backoff_base_seconds":
		return strconv.Itoa(cfg.API.CircuitBackoffBase)
	case "api.miss_backoff_base_hours":
		return strconv.Itoa(cfg.API.MissBackoffBaseHours)
	case "api.miss_backoff_cap_hours":
		return strconv.Itoa(cfg.API.MissBackoffCapHours)
	case "api.max_miss_attempts":
		return strconv.Itoa(cfg.API.MaxMissAttempts)
	// [output]
	case "output.dir":
		return cfg.Output.Dir
	case "output.embedded_lyrics":
		return cfg.Output.EmbeddedLyrics
	case "output.bilingual_output":
		return strconv.FormatBool(cfg.Output.BilingualOutput)
	// [db]
	case "db.path":
		return cfg.DB.Path
	// [secrets]
	case "secrets.key_file":
		return cfg.Secrets.KeyFile
	// [server]
	case "server.addr":
		return cfg.Server.Addr
	case "server.scan_interval_seconds":
		return strconv.Itoa(cfg.Server.ScanIntervalSeconds)
	case "server.work_interval_seconds":
		return strconv.Itoa(cfg.Server.WorkIntervalSeconds)
	case "server.trusted_networks.cidrs":
		return joinSlice(cfg.Server.TrustedNetworks.Cidrs)
	case "server.trusted_networks.trusted_proxies":
		return joinSlice(cfg.Server.TrustedNetworks.TrustedProxies)
	case "server.tls.cert_file":
		return cfg.Server.TLS.CertFile
	case "server.tls.key_file":
		return cfg.Server.TLS.KeyFile
	case "server.tls.self_signed":
		return strconv.FormatBool(cfg.Server.TLS.SelfSigned)
	case "server.tls.redirect_http":
		return cfg.Server.TLS.RedirectHTTP
	case "server.tls.self_signed_hosts":
		return joinSlice(cfg.Server.TLS.SelfSignedHosts)
	// [providers]
	case "providers.primary":
		return cfg.Providers.Primary
	case "providers.disabled":
		return joinSlice(cfg.Providers.Disabled)
	case "providers.mode":
		return cfg.Providers.Mode
	case "providers.race_wait_seconds":
		return strconv.Itoa(cfg.Providers.RaceWaitSeconds)
	case "providers.fallback_order":
		return joinSlice(cfg.Providers.FallbackOrder)
	// [verification]
	case "verification.enabled":
		return strconv.FormatBool(cfg.Verification.Enabled)
	case "verification.whisper_url":
		return cfg.Verification.WhisperURL
	case "verification.ffmpeg_path":
		return cfg.Verification.FFmpegPath
	case "verification.sample_duration_seconds":
		return strconv.Itoa(cfg.Verification.SampleDurationSeconds)
	case "verification.min_confidence":
		return formatFloat(cfg.Verification.MinConfidence)
	case "verification.min_similarity":
		return formatFloat(cfg.Verification.MinSimilarity)
	// [instrumental_detector]
	case "instrumental_detector.enabled":
		return strconv.FormatBool(cfg.InstrumentalDetector.Enabled)
	case "instrumental_detector.classifier_url":
		return cfg.InstrumentalDetector.ClassifierURL
	case "instrumental_detector.ffmpeg_path":
		return cfg.InstrumentalDetector.FFmpegPath
	case "instrumental_detector.sample_duration_seconds":
		return strconv.Itoa(cfg.InstrumentalDetector.SampleDurationSeconds)
	case "instrumental_detector.min_confidence":
		return formatFloat(cfg.InstrumentalDetector.MinConfidence)
	case "instrumental_detector.instrumental_classes":
		return joinSlice(cfg.InstrumentalDetector.InstrumentalClasses)
	case "instrumental_detector.cooldown_seconds":
		return strconv.Itoa(cfg.InstrumentalDetector.CooldownSeconds)
	// [enrichment]
	case "enrichment.enabled":
		return strconv.FormatBool(cfg.Enrichment.Enabled)
	// [guard]
	case "guard.accepted_scripts":
		return joinSlice(cfg.Guard.AcceptedScripts)
	case "guard.script_guard_threshold":
		return formatFloat(cfg.Guard.Threshold)
	// [queue]
	case "queue.randomize":
		return strconv.FormatBool(cfg.Queue.Randomize)
	// [logging]
	case "logging.level":
		return cfg.Logging.Level
	case "logging.format":
		return cfg.Logging.Format
	case "logging.file":
		return cfg.Logging.File
	case "logging.max_size_mb":
		return strconv.Itoa(cfg.Logging.MaxSizeMB)
	case "logging.max_files":
		return strconv.Itoa(cfg.Logging.MaxFiles)
	case "logging.max_age_days":
		return strconv.Itoa(cfg.Logging.MaxAgeDays)
	case "logging.compress":
		return strconv.FormatBool(cfg.Logging.Compress)
	}
	return ""
}

// joinSlice renders a string slice comma-joined, or "[]" when empty, matching
// the Config view's slice convention.
func joinSlice(vals []string) string {
	if len(vals) == 0 {
		return "[]"
	}
	return strings.Join(vals, ", ")
}

// formatFloat renders a float the same way the Config view does (%g: shortest
// representation that round-trips).
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// inputType maps a registry FieldType to the template's input-rendering hint.
func inputType(t config.FieldType) string {
	switch t {
	case config.TypeInt:
		return "int"
	case config.TypeBool:
		return "bool"
	case config.TypeFloat64:
		return "float"
	case config.TypeStringSlice:
		return "slice"
	default:
		return "text"
	}
}

// settingsAcronyms upper-cases initialisms in field labels so "tls" reads "TLS"
// rather than "Tls".
var settingsAcronyms = map[string]string{
	"api":   "API",
	"db":    "DB",
	"tls":   "TLS",
	"url":   "URL",
	"cidrs": "CIDRs",
	"http":  "HTTP",
	"mb":    "MB",
}

// settingsLabel returns the field's plain-language label. The curated
// settingsLabels map is the source of truth (no dotted config keys reach the
// UI); a path missing from it falls back to a humanized path segment so a newly
// added field still renders a sensible label (caught by
// TestSettingsLabelsCoverRegistry).
func settingsLabel(spec config.FieldSpec) string {
	if l, ok := settingsLabels[spec.Path]; ok {
		return l
	}
	rest := strings.TrimPrefix(spec.Path, spec.Section+".")
	rest = strings.NewReplacer("_", " ", ".", " ").Replace(rest)
	words := strings.Fields(rest)
	for i, w := range words {
		if up, ok := settingsAcronyms[w]; ok {
			words[i] = up
			continue
		}
		if i == 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// settingsLabels is the curated plain-language field label, keyed by config
// path. Everyday words, no dotted keys or jargon; the Common-tab entries use the
// exact labels from the #288 UAT. A field absent here falls back to a humanized
// path segment in settingsLabel.
var settingsLabels = map[string]string{
	// Common tab (exact UAT labels).
	"api.token":                    "Musixmatch token",
	"api.cooldown":                 "Seconds to wait between requests",
	"output.dir":                   "Where to save lyrics",
	"output.embedded_lyrics":       "What to do with lyrics already in the file",
	"output.bilingual_output":      "Save the original and the translation together",
	"providers.primary":            "Main lyrics source",
	"providers.mode":               "How to use multiple sources",
	"server.addr":                  "Web page address",
	"server.webhook_api_keys":      "Webhook keys",
	"server.scan_interval_seconds": "How often to scan the library (seconds)",
	"enrichment.enabled":           "Look up extra track info first",
	"logging.level":                "How much detail to log",
	// Advanced tab.
	"api.circuit_open_duration":                     "Max pause after repeated rate-limiting (seconds)",
	"api.circuit_backoff_base_seconds":              "First pause after rate-limiting (seconds)",
	"api.miss_backoff_base_hours":                   "First re-check delay after a miss (hours)",
	"api.miss_backoff_cap_hours":                    "Longest re-check delay after a miss (hours)",
	"api.max_miss_attempts":                         "Give up after this many misses (0 means never)",
	"db.path":                                       "Database file location",
	"secrets.key_file":                              "Secret key file location",
	"server.work_interval_seconds":                  "How often to process the queue (seconds)",
	"server.trusted_networks.cidrs":                 "Client networks allowed to connect",
	"server.trusted_networks.trusted_proxies":       "Trusted proxy networks",
	"server.tls.cert_file":                          "HTTPS certificate file",
	"server.tls.key_file":                           "HTTPS private key file",
	"server.tls.self_signed":                        "Use a self-signed HTTPS certificate",
	"server.tls.redirect_http":                      "Plain web address to redirect to HTTPS",
	"server.tls.self_signed_hosts":                  "Extra host names for the self-signed certificate",
	"providers.disabled":                            "Lyrics sources to use",
	"providers.race_wait_seconds":                   "Wait for a better match (seconds)",
	"providers.fallback_order":                      "Which source to try first, second, ...",
	"verification.enabled":                          "Check that lyrics match the audio",
	"verification.whisper_url":                      "Transcription service address",
	"verification.ffmpeg_path":                      "ffmpeg program location",
	"verification.sample_duration_seconds":          "Audio sample length to check (seconds)",
	"verification.min_confidence":                   "Minimum transcription confidence (0-1)",
	"verification.min_similarity":                   "Minimum lyric match similarity (0-1)",
	"instrumental_detector.enabled":                 "Detect instrumental tracks",
	"instrumental_detector.classifier_url":          "Audio classifier service address",
	"instrumental_detector.ffmpeg_path":             "ffmpeg program location",
	"instrumental_detector.sample_duration_seconds": "Audio sample length to check (seconds)",
	"instrumental_detector.min_confidence":          "Minimum detection confidence (0-1)",
	"instrumental_detector.instrumental_classes":    "Sounds that count as instrumental",
	"instrumental_detector.cooldown_seconds":        "Wait between detector checks (seconds)",
	"guard.accepted_scripts":                        "Writing systems to accept without asking",
	"guard.script_guard_threshold":                  "Foreign-script sensitivity (0-1)",
	"queue.randomize":                               "Process tracks in random order",
	"logging.format":                                "Log format (text or json)",
	"logging.file":                                  "Log file location (blank logs to the screen)",
	"logging.max_size_mb":                           "Rotate the log after this size (MB)",
	"logging.max_files":                             "How many old log files to keep",
	"logging.max_age_days":                          "Delete old log files after this many days",
	"logging.compress":                              "Compress old log files",
}

// settingsDescriptions is the per-field help line shown under each label. Kept
// here (not in the registry) so copy edits do not churn the shared config
// package. Plain language, one line each.
var settingsDescriptions = map[string]string{ //nolint:gosec // G101 false positive: help-text copy keyed by config paths (incl. "api.token"); no hardcoded credential
	"api.token":                                     "Musixmatch API token. Required to fetch lyrics.",
	"api.cooldown":                                  "Minimum seconds between Musixmatch requests.",
	"api.circuit_open_duration":                     "Maximum circuit-breaker pause (seconds) after repeated throttling.",
	"api.circuit_backoff_base_seconds":              "Initial circuit-breaker pause (seconds); doubles on each trip.",
	"api.miss_backoff_base_hours":                   "Initial re-check delay (hours) for a benign miss.",
	"api.miss_backoff_cap_hours":                    "Maximum re-check delay (hours) for a benign miss.",
	"api.max_miss_attempts":                         "Retire a queue row after this many misses. 0 disables the cap.",
	"output.dir":                                    "Default directory for .lrc output files.",
	"output.embedded_lyrics":                        "How to handle embedded lyrics: off, respect, or extract.",
	"output.bilingual_output":                       "Interleave original and translation lines in one .lrc.",
	"db.path":                                       "SQLite database file path.",
	"secrets.key_file":                              "Path to the 32-byte master key that encrypts stored secrets (the API token and webhook keys). Defaults to .mxlrcgo.key beside the database, or set MXLRC_MASTER_KEY to supply the key directly. Read-only here: the key cannot be rotated while the server is running.",
	"server.addr":                                   "Listen address for serve mode (host:port).",
	"server.webhook_api_keys":                       "Keys that Lidarr sends to authenticate its webhook to this server (POST /api/v1/webhooks/lidarr). Only Lidarr is supported today.",
	"server.scan_interval_seconds":                  "Seconds between library scans in serve mode.",
	"server.work_interval_seconds":                  "Seconds between work-queue drains in serve mode.",
	"server.trusted_networks.cidrs":                 "Client CIDRs allowed to reach the server.",
	"server.trusted_networks.trusted_proxies":       "Proxy CIDRs whose forwarded-for header is trusted.",
	"server.tls.cert_file":                          "TLS certificate file path (PEM-encoded, typically .pem or .crt). Enter this together with the private key below and click Save on either - the pair is written in one step. Leave both blank (and turn on self-signed) if you don't have a custom certificate.",
	"server.tls.key_file":                           "TLS private key file path (PEM-encoded, typically .key). Enter this together with the certificate above and click Save on either - the pair is written in one step.",
	"server.tls.self_signed":                        "Generate and use a self-signed certificate.",
	"server.tls.redirect_http":                      "HTTP listen address to redirect to HTTPS (blank disables).",
	"server.tls.self_signed_hosts":                  "Hostnames to include in the self-signed certificate.",
	"providers.primary":                             "Lyrics provider tried first.",
	"providers.disabled":                            "Check each lyrics source to enable it. Either or both may run; checking Musixmatch un-greys its API token field. An unchecked source is never used. The primary source can't be disabled here - change the main source first.",
	"providers.mode":                                "How to use multiple sources: ordered (try in turn) or parallel (race them).",
	"providers.race_wait_seconds":                   "Grace seconds the race winner waits for a richer result.",
	"providers.fallback_order":                      "Provider order tried in fallback mode.",
	"verification.enabled":                          "Verify fetched lyrics against the audio.",
	"verification.whisper_url":                      "Whisper transcription service URL.",
	"verification.ffmpeg_path":                      "Path to the ffmpeg binary used to sample audio.",
	"verification.sample_duration_seconds":          "Seconds of audio sampled for verification.",
	"verification.min_confidence":                   "Minimum transcription confidence to accept (0-1).",
	"verification.min_similarity":                   "Minimum lyric/transcript similarity to accept (0-1).",
	"instrumental_detector.enabled":                 "Detect instrumental tracks via an audio classifier.",
	"instrumental_detector.classifier_url":          "Audio-classifier service URL.",
	"instrumental_detector.ffmpeg_path":             "Path to the ffmpeg binary used to sample audio.",
	"instrumental_detector.sample_duration_seconds": "Seconds of audio sampled for detection.",
	"instrumental_detector.min_confidence":          "Minimum classifier confidence to mark instrumental (0-1).",
	"instrumental_detector.instrumental_classes":    "Google AudioSet classifier labels that count as instrumental (defaults: Music, Musical instrument).",
	"instrumental_detector.cooldown_seconds":        "Seconds to wait between detector calls.",
	"enrichment.enabled":                            "Look up recording IDs (ISRC, MusicBrainz MBID, Spotify ID) before fetching, and feed them to the matcher to improve match accuracy.",
	"guard.accepted_scripts":                        "The foreign-script guard skips or flags lyrics whose share of characters outside these accepted writing systems exceeds the sensitivity threshold below. Valid buckets: Latin, Han, Kana, Hangul, Other. Empty disables the guard.",
	"guard.script_guard_threshold":                  "The fraction of non-accepted-script characters that trips the guard (0-1). Higher is more tolerant (more foreign text allowed); lower is stricter.",
	"queue.randomize":                               "Process queued tracks in random order.",
	"logging.level":                                 "Log verbosity: debug, info, warn, or error. This single level governs both console and file output (there is no separate per-sink level).",
	"logging.format":                                "Log format: text or json.",
	"logging.file":                                  "Where to write logs. Leave blank to log to the console (stderr).",
	"logging.max_size_mb":                           "Rotate the log file after this many megabytes.",
	"logging.max_files":                             "Number of rotated log files to keep.",
	"logging.max_age_days":                          "Delete rotated log files older than this many days.",
	"logging.compress":                              "Gzip rotated log files.",
}
