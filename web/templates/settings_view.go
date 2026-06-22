package templates

import "strconv"

// Presentation models for the Settings page (#288). The handler maps the
// config.Registry() FieldSpec entries plus the effective config onto these
// string-bearing view structs so the templ files stay free of formatting and
// config-package concerns. Every value is pre-formatted (and redacted, for
// secrets) by the handler; templates only branch on the carried flags.
//
// Phase 1 is the READ path: the page renders current values, lock status, and
// metadata. The write handlers and a real submit target arrive in Phase 2.

// SettingsView is the top-level Settings page view model. The page is three
// tabs: Common (the handful of everyday fields, in a fixed order), Advanced
// (every other field grouped by section), and Raw config (the current config
// file rendered read-only as TOML, secrets redacted).
type SettingsView struct {
	// Common is the everyday field set shown on the default tab, in a fixed
	// order (not grouped by section).
	Common []SettingsField
	// Sections are the Advanced-tab fields grouped by config section, excluding
	// every field already shown on the Common tab.
	Sections []SettingsSection
	// RawTOML is the effective config rendered as TOML with secrets redacted,
	// shown on the "Config file" tab's Effective toggle.
	RawTOML string
	// RawFileTOML is the literal content of the config file on disk with secrets
	// redacted, shown on the "Config file" tab's Raw toggle. Empty when no config
	// file path is configured.
	RawFileTOML string
	// CSRFToken is the double-submit token embedded in the page for the save
	// POSTs; settings.js sends it as the csrf_token field. Empty when the write
	// path is not wired (the page is then read-only).
	CSRFToken string
	// Writable is true when the settings write path is wired (a config file path
	// is set); the template renders save controls only then.
	Writable bool
}

// SettingsSection is one top-level config section (e.g. "api") with its fields.
type SettingsSection struct {
	// Key is the TOML section key (e.g. "api"); used as a stable id prefix.
	Key string
	// Title is the human label shown as the section heading.
	Title string
	// Fields are the section's fields in registry order.
	Fields []SettingsField
}

// SettingsField is one config field's read-path view model. It carries every
// metadatum the Settings page shows for a field: its identity, help text, the
// current effective value (redacted when sensitive), and the lock/criticality
// status that drives the input affordance.
type SettingsField struct {
	// Path is the dotted config key (e.g. "api.token"). Unique across the page.
	Path string
	// DOMID is the unique element id for the field's input and label binding
	// (the path with dots replaced by dashes, e.g. "field-api-token").
	DOMID string
	// Label is the human field label.
	Label string
	// Description is one line of guidance shown under the label.
	Description string
	// InputType is the rendering hint for the control: "text", "int", "float",
	// "slice", "bool" (radio pair), "select" (dropdown), "providers" (provider
	// enable checkboxes), "ordered" (ordered pick list), "taglist" (add/remove
	// address list), "duration" (number + unit), "secret" (password), or
	// "webhook" (key field + generate button).
	InputType string
	// EffectiveValue is the current merged value, pre-formatted as a string and
	// redacted when Sensitive. For an unlocked field this is also the file value
	// (no override is in effect), so it doubles as the input prefill.
	EffectiveValue string
	// Options carries the choices for bool / select / multiselect / ordered
	// fields, each pre-marked Selected against the current value.
	Options []SettingsOption
	// DisplayValue is a duration field's current value expressed in DisplayUnit
	// (e.g. 5 when the canonical value is 300 seconds and DisplayUnit is
	// "minutes"). Empty for non-duration fields.
	DisplayValue string
	// DisplayUnit is a duration field's natural unit ("seconds"/"minutes"/
	// "hours"/"days"); UnitOptions are the units offered in the unit dropdown.
	DisplayUnit string
	UnitOptions []string
	// GenPrefix is the key prefix a "Generate key" button prepends to a random
	// suffix (webhook field only); sourced from auth.KeyPrefix.
	GenPrefix string
	// ListValues are the current entries of an address-list (taglist) field,
	// each rendered as an individually-removable item. Empty for other fields.
	ListValues []string
	// Placeholder is the input placeholder for a taglist field (e.g. an example
	// CIDR). Empty for other fields.
	Placeholder string
	// EnableWhenChecked is the DOM id of the checkbox/radio whose checked state
	// enables (un-greys) this field. Empty when the field is not gated. The
	// gating is client-side only (settings.js); see enableController.
	EnableWhenChecked string
	// JumpTargetID / JumpTab / JumpLabel render an in-page link under the control
	// that switches to JumpTab and scrolls to JumpTargetID (e.g. from the mode
	// field to the fallback-order section). All empty when there is no jump link.
	JumpTargetID string
	JumpTab      string
	JumpLabel    string
	// Locked is true when an environment variable (or CLI flag) overrides the
	// file value, so the field is read-only until the override is removed. The
	// card shows a "Locked" pill; hover reveals LockSource when set (#307).
	Locked bool
	// LockSource is the winning env var name when Locked is true (e.g.
	// "MXLRC_API_TOKEN"). Empty when the field is not locked. Rendered as a
	// title= tooltip on the Locked pill so operators know which var to clear (#307).
	LockSource string
	// Sensitive marks a secret field: never echoes the stored secret.
	Sensitive bool
	// Editable is false for fields the writer must never rewrite (only
	// secrets.key_file); such a field renders as a read-only display, not an
	// input.
	Editable bool
	// Tier is the field's risk tier ("safe" | "caution" | "critical") from the
	// registry Criticality, driving the save trigger: safe hot-saves on change,
	// caution needs an explicit Save, critical needs Save + a confirm dialog.
	Tier string
	// Savable is true when this field can be written from the page: the write
	// path is wired (view.Writable), the field is editable, and it is not locked
	// by an env override. The template renders save controls only then.
	Savable bool
	// SaveGroup, when non-empty, marks this field as part of a paired/section
	// save (#298): clicking Save on any card in the group POSTs every group
	// member's value together to /settings/section as one atomic change, so a
	// cross-field invariant (the [server.tls] cert+key pair) can be satisfied from
	// an empty state where a single-field save would always 400. Empty for the
	// normal one-field save path.
	SaveGroup string
}

// SettingsOption is one choice for a bool / select / multiselect / ordered
// field. For an ordered pick list the Label already carries the position prefix
// (e.g. "1. musixmatch").
type SettingsOption struct {
	Value    string
	Label    string
	Selected bool
	// Fixed marks a choice the user cannot toggle off (rendered checked +
	// disabled), e.g. the primary provider in the enablement list - it can't be
	// disabled here without first changing the primary.
	Fixed bool
}

// OptionID returns the element id for the i-th option input in a group: the
// field's DOMID for the first (so the card label's for-binding lands on a real
// control), and a suffixed id for the rest, keeping every id unique.
func OptionID(domid string, i int) string {
	if i == 0 {
		return domid
	}
	return domid + "-" + strconv.Itoa(i)
}
