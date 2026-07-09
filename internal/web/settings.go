package web

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/doxazo-net/canticle/internal/auth"
	"github.com/doxazo-net/canticle/internal/config"
	"github.com/doxazo-net/canticle/internal/providers"
	"github.com/doxazo-net/canticle/web/templates"
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
	render(w, r, templates.SettingsPage(u.version, view, u.buildRail(""), u.musixmatchInactive))
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
	{"watcher", "Watcher"},
	{"providers", "Providers"},
	{"verification", "Verification"},
	{"instrumental_detector", "Instrumental Detector"},
	{"enrichment", "Enrichment"},
	{"realign", "Realign"},
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

	// The raw on-disk view distinguishes three states: a read error (path wired
	// but unreadable), the rendered file, and the unconfigured state (no path).
	// A read error sets RawFileTOMLError so the template renders it distinctly
	// from the empty/unconfigured case rather than silently showing a blank view.
	rawFileTOML, rawFileErr := u.buildRawFileTOML()
	view := templates.SettingsView{
		// FormatConfigText is the single redaction source of truth (shared with
		// the logging layer); secrets are masked before the text reaches the
		// template. Source-hint maps are nil: the Config file tab shows merged
		// effective values, not per-field provenance.
		RawTOML:     annotateRawConfig(config.FormatConfigText(cfg, nil, nil)),
		RawFileTOML: rawFileTOML,
	}
	if rawFileErr != nil {
		// Surface a generic message to the page (the underlying error may name the
		// on-disk path, which the read-only view should not expose); log the detail.
		slog.Error("settings: reading raw config file failed", "error", rawFileErr)
		view.RawFileTOMLError = "The config file could not be read."
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
		Description: spec.Description,
		Sensitive:   spec.Sensitive,
		Editable:    spec.Editable,
		Tier:        criticalityTier(spec.Criticality),
	}

	// Lock status: the field is locked when one of its own env vars is set
	// non-empty (an active override). CLI overrides are not visible to a
	// long-running daemon's process env, so the env presence check is the
	// read-path signal. fieldEnvLockSource also returns the winning var name so
	// the template can surface it on the Locked pill tooltip (#307).
	f.LockSource, f.Locked = fieldEnvLockSource(spec)

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
	"realign.enabled":               {"Re-attach orphaned lyric files", "Off"},
	"realign.on_scan":               {"Realign after every scan", "Only when run manually"},
	"realign.require_provenance":    {"Only move on an exact ID match", "Allow name-based matches too"},
	"realign.cross_directory":       {"Allow matches across directories", "Same directory only"},
	"queue.randomize":               {"Process in random order", "Process in order"},
	"watcher.enabled":               {"Watch for new files", "Don't watch"},
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

// buildRawFileTOML reads the config file on disk and returns its contents with
// secrets redacted (#319 Raw toggle). It distinguishes two empty-string cases by
// the error return: a nil error with an empty string means no config path is
// wired (the unconfigured state), while a non-nil error means the path is wired
// but the file could not be read (a genuine read failure the page must surface
// distinctly, #367).
func (u *UI) buildRawFileTOML() (string, error) {
	if u.configPath == "" {
		return "", nil
	}
	data, err := os.ReadFile(u.configPath)
	if err != nil {
		return "", fmt.Errorf("read config file: %w", err)
	}
	return redactRawTOML(string(data)), nil
}

// redactRawTOML replaces the value of sensitive TOML keys (those whose registry
// entry carries Sensitive: true) with "(redacted)", so the raw file view never
// exposes the stored secret text. It tracks the current section header to match
// the section+key pair against the registry, and preserves a sensitive line's
// leading indentation, key token, and any trailing inline comment verbatim
// (#367): only the value is replaced.
func redactRawTOML(raw string) string {
	sensitive := map[string]bool{}
	for _, spec := range config.Registry() {
		if spec.Sensitive {
			sensitive[spec.Path] = true
		}
	}

	var b strings.Builder
	section := ""
	lines := strings.Split(raw, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		// Skip comment lines verbatim so they are never treated as key/value
		// pairs or section headers.
		if strings.HasPrefix(trimmed, "#") {
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		// Section headers cover both [table] and [[array-of-tables]]; trimming
		// all surrounding brackets yields the dotted-section name in either case.
		// normalizeTOMLKeyPath unquotes any quoted segments so a header written as
		// ["server"."tls"] resolves to the same dotted name as [server.tls].
		//
		// A header may carry a trailing inline comment ([server] # note), which is
		// valid TOML. Strip the comment (quote-aware, so a '#' inside a quoted key
		// is kept) BEFORE testing for the [...] shape, otherwise the trimmed line
		// ends with "note" instead of "]" and the section would not update -- a
		// following sensitive key would then be keyed at top level, miss the
		// registry match, and leak its value (#367).
		headerCandidate := trimmed
		if comment := trailingTOMLComment(trimmed); comment != "" {
			headerCandidate = strings.TrimRight(trimmed[:len(trimmed)-len(comment)], " \t")
		}
		if strings.HasPrefix(headerCandidate, "[") && strings.HasSuffix(headerCandidate, "]") {
			section = normalizeTOMLKeyPath(strings.Trim(headerCandidate, "[]"))
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		// Locate the key/value split on the first '=' regardless of surrounding
		// whitespace, so key=value and key = value are both redacted. The key
		// region (everything left of '=') may itself be a dotted and/or quoted
		// path (e.g. tls.cert_file or "token"); normalize it to the registry's
		// dotted form before matching.
		if eq := strings.IndexByte(line, '='); eq > 0 {
			keyPath := normalizeTOMLKeyPath(line[:eq])
			full := keyPath
			if section != "" {
				full = section + "." + keyPath
			}
			if sensitive[full] {
				// Preserve the original indentation + key token (trim only the
				// run of whitespace before '='), redact the value, and re-attach
				// any trailing inline comment verbatim.
				b.WriteString(strings.TrimRight(line[:eq], " \t"))
				b.WriteString(` = "(redacted)"`)
				if comment := trailingTOMLComment(line[eq+1:]); comment != "" {
					b.WriteByte(' ')
					b.WriteString(comment)
				}
				b.WriteByte('\n')
				// If the sensitive value opens a TOML array that is not closed on
				// this line, its real elements live on the continuation lines.
				// The placeholder above already stands in for the whole value, so
				// drop every continuation line until the array's brackets balance
				// (the closing ']' line is part of the value and is dropped too).
				// Without this, a multi-line array of secrets would leak every
				// element after the first verbatim.
				value := line[eq+1:]
				depth := tomlBracketDepth(value, 0)
				for depth > 0 && i+1 < len(lines) {
					i++
					depth = tomlBracketDepth(lines[i], depth)
				}
				// A sensitive value may instead open a multi-line basic ("""...""")
				// or literal ('''...''') string whose body spans later lines. The
				// placeholder stands in for the whole string, so drop continuation
				// lines until the matching closing delimiter. Without this, every
				// body line after the opener (the secret) would render verbatim. A
				// triple-quote that also closes on the opening line is not consumed.
				if delim, open := openMultilineTOMLString(value); open {
					for i+1 < len(lines) {
						i++
						if strings.Contains(lines[i], delim) {
							break
						}
					}
				}
				continue
			}
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// normalizeTOMLKeyPath turns a TOML key path or section name into its canonical
// dotted form: it splits on '.', then strips surrounding whitespace and matched
// quotes from each segment, so api.token, "api"."token", and ' api . token '
// all normalize to "api.token". This lets the redaction match a quoted or
// loosely-spaced key against the registry's plain dotted paths.
func normalizeTOMLKeyPath(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 2 {
			if (p[0] == '"' && p[len(p)-1] == '"') || (p[0] == '\'' && p[len(p)-1] == '\'') {
				p = p[1 : len(p)-1]
			}
		}
		parts[i] = p
	}
	return strings.Join(parts, ".")
}

// trailingTOMLComment returns the trailing inline comment (including its leading
// '#') from a TOML value region (the text to the right of '='), or "" when there
// is none. It is quote-aware: a '#' inside a "basic" or 'literal' string is part
// of the value, not a comment, so key = "a#b" has no comment. This lets the
// redaction strip a sensitive value while keeping its trailing comment verbatim.
func trailingTOMLComment(after string) string {
	inBasic := false   // inside a "..." basic string
	inLiteral := false // inside a '...' literal string
	for i := 0; i < len(after); i++ {
		c := after[i]
		switch {
		case inBasic:
			switch c {
			case '\\':
				i++ // skip the escaped character (e.g. \" does not close the string)
			case '"':
				inBasic = false
			}
		case inLiteral:
			// Literal strings have no escapes; only a closing quote ends them.
			if c == '\'' {
				inLiteral = false
			}
		default:
			switch c {
			case '"':
				inBasic = true
			case '\'':
				inLiteral = true
			case '#':
				return after[i:]
			}
		}
	}
	return ""
}

// tomlBracketDepth returns the running TOML array-bracket nesting depth after
// scanning s, starting from the given depth. It is quote-aware in the same way
// as trailingTOMLComment: a '[' or ']' inside a "basic" or 'literal' string is
// part of the value text and does not change the depth (so a secret element
// like "a]b" does not falsely close the array). A '#' outside any string starts
// a comment that ends the line, so brackets after it are ignored. The redaction
// uses this to find where a sensitive multi-line array value closes.
func tomlBracketDepth(s string, depth int) int {
	inBasic := false   // inside a "..." basic string
	inLiteral := false // inside a '...' literal string
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inBasic:
			switch c {
			case '\\':
				i++ // skip the escaped character (e.g. \" does not close the string)
			case '"':
				inBasic = false
			}
		case inLiteral:
			// Literal strings have no escapes; only a closing quote ends them.
			if c == '\'' {
				inLiteral = false
			}
		default:
			switch c {
			case '"':
				inBasic = true
			case '\'':
				inLiteral = true
			case '#':
				return depth // rest of the line is a comment
			case '[':
				depth++
			case ']':
				depth--
			}
		}
	}
	return depth
}

// openMultilineTOMLString reports whether a TOML value region (the text right
// of '=') opens a multi-line basic (""") or literal (”') string that is NOT
// closed on the same line, returning the opening delimiter and true in that
// case. A triple-quote that also closes on the opening line (token = """x""")
// is fully contained and returns false, so the redaction does not swallow the
// following lines. The redaction uses this to drop the secret body of a
// sensitive multi-line string until its closing delimiter.
func openMultilineTOMLString(value string) (string, bool) {
	v := strings.TrimLeft(value, " \t")
	for _, delim := range []string{`"""`, `'''`} {
		if strings.HasPrefix(v, delim) {
			// Closed on this same line if the delimiter appears again after the
			// opener; otherwise the body continues on later lines.
			if strings.Contains(v[len(delim):], delim) {
				return delim, false
			}
			return delim, true
		}
	}
	return "", false
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
	// [realign]
	case "realign.enabled":
		return strconv.FormatBool(cfg.Realign.Enabled)
	case "realign.on_scan":
		return strconv.FormatBool(cfg.Realign.OnScan)
	case "realign.require_provenance":
		return strconv.FormatBool(cfg.Realign.RequireProvenance)
	case "realign.cross_directory":
		return strconv.FormatBool(cfg.Realign.CrossDirectory)
	case "realign.identity_keys":
		return joinSlice(cfg.Realign.IdentityKeys)
	case "realign.min_confidence":
		return formatFloat(cfg.Realign.MinConfidence)
	// [guard]
	case "guard.accepted_scripts":
		return joinSlice(cfg.Guard.AcceptedScripts)
	case "guard.script_guard_threshold":
		return formatFloat(cfg.Guard.Threshold)
	// [queue]
	case "queue.randomize":
		return strconv.FormatBool(cfg.Queue.Randomize)
	// [watcher]
	case "watcher.enabled":
		return strconv.FormatBool(cfg.Watcher.Enabled)
	case "watcher.debounce_ms":
		return strconv.Itoa(cfg.Watcher.DebounceMS)
	case "watcher.max_dirs":
		return strconv.Itoa(cfg.Watcher.MaxDirs)
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
	"server.web_ui_enabled":        "Show the web page",
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
	"instrumental_detector.vocal_classes":           "Vocal sounds that rule out instrumental",
	"instrumental_detector.vocal_max_confidence":    "Vocal-peak limit to still count as instrumental (0-1)",
	"instrumental_detector.speech_classes":          "Speech sounds gated on sustained presence, not brief peaks",
	"instrumental_detector.speech_max_confidence":   "Sustained-speech limit to still count as instrumental (0-1)",
	"instrumental_detector.spread_samples":          "Number of samples taken across the track",
	"instrumental_detector.ffprobe_path":            "ffprobe program location",
	"instrumental_detector.cooldown_seconds":        "Wait between detector checks (seconds)",
	"realign.enabled":                               "Re-attach orphaned lyric files",
	"realign.on_scan":                               "Realign automatically after each scan",
	"realign.require_provenance":                    "Require an exact ID match to move a file",
	"realign.cross_directory":                       "Allow moves across directories",
	"realign.identity_keys":                         "Which IDs to match on (in order)",
	"realign.min_confidence":                        "Minimum name-match similarity for a guess (0-1)",
	"guard.accepted_scripts":                        "Writing systems to accept without asking",
	"guard.script_guard_threshold":                  "Foreign-script sensitivity (0-1)",
	"queue.randomize":                               "Process tracks in random order",
	"watcher.enabled":                               "Watch for new files",
	"watcher.debounce_ms":                           "Quiet period after file changes (milliseconds)",
	"watcher.max_dirs":                              "Maximum directories to watch",
	"logging.format":                                "Log format (text or json)",
	"logging.file":                                  "Log file location (blank logs to the screen)",
	"logging.max_size_mb":                           "Rotate the log after this size (MB)",
	"logging.max_files":                             "How many old log files to keep",
	"logging.max_age_days":                          "Delete old log files after this many days",
	"logging.compress":                              "Compress old log files",
}

// Per-field help text now lives on the config registry (FieldSpec.Description),
// the single source of truth shared with the config.toml comment stamping on
// save (#291). settingsField reads spec.Description directly.
