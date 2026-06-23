package templates

// Presentation models for the webhook API key management page (#300). The
// handler maps auth.Key metadata onto these string-bearing view structs so the
// templ file stays free of formatting and auth-package concerns.
//
// Security invariant: a key's raw material is shown EXACTLY ONCE, in NewKey,
// immediately after creation. The persisted list (Keys) carries only metadata -
// never the raw key and never the full PBKDF2 hash. The displayed ID is the
// public Key.ID (the hash prefix), the same identifier the `keys list` CLI
// prints.

// WebhookKeysView is the top-level view model for the webhook keys page.
type WebhookKeysView struct {
	// Keys is the masked list of existing keys, newest activity last (creation
	// order), each carrying only display metadata.
	Keys []WebhookKeyRow
	// NewKey is non-nil only on the response that just created a key: it carries
	// the one-time raw key to display once. It is never set when rendering the
	// list on its own, so the raw key is shown exactly once and never re-rendered.
	NewKey *NewWebhookKey
	// ScopeOptions are the scope checkboxes offered on the create form.
	ScopeOptions []WebhookScopeOption
	// CSRFToken is the double-submit token embedded in the create/revoke forms.
	// Empty when key management is not wired (the page is then read-only).
	CSRFToken string
	// Manageable is true when the key-management backend is wired, so the create
	// and revoke controls render. False renders an unavailable notice instead.
	Manageable bool
	// Error is a user-facing message shown when listing, creating, or revoking
	// failed. Empty on success.
	Error string
}

// WebhookKeyRow is one existing key's masked display row. It deliberately omits
// the full hash and any raw material.
type WebhookKeyRow struct {
	// ID is the truncated public ID for display (e.g. "a1b2c3d4…").
	ID string
	// FullID is the complete public ID (auth.Key.ID), used as the revoke target.
	// It is NOT the hash and carries no secret material.
	FullID string
	// Name is the operator-assigned label, or a placeholder when unnamed.
	Name string
	// Scopes is the comma-joined scope list (e.g. "webhook").
	Scopes string
	// Created is the server-side formatted creation timestamp (display string).
	Created string
	// CreatedISO is the RFC3339 UTC value for the <time datetime=> attribute, so
	// the client reformats it to the viewer's local zone (empty for a zero time).
	CreatedISO string
	// CreatedTZApplied is true when the server already applied a TZ-env zone, so
	// the client should not reformat (mirrors the dashboard pattern).
	CreatedTZApplied bool
	// Revoked is true once the key has been revoked.
	Revoked bool
	// RevokedAt / RevokedAtISO / RevokedAtTZApplied are the revocation timestamp's
	// display string, ISO value, and tz-applied flag, shown when Revoked.
	RevokedAt          string
	RevokedAtISO       string
	RevokedAtTZApplied bool
}

// NewWebhookKey carries the one-time raw key shown immediately after creation.
type NewWebhookKey struct {
	// Raw is the full raw key string. Shown once; never persisted to the rendered
	// list afterwards.
	Raw string
	// Name is the label the key was created with (may be empty).
	Name string
	// ID is the new key's public ID, so the operator can correlate it with the
	// list row.
	ID string
}

// WebhookScopeOption is one scope checkbox on the create form.
type WebhookScopeOption struct {
	Value   string
	Label   string
	Checked bool
}
