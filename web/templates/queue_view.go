package templates

import "strconv"

// QueueView is the presentation model for one /queue/{bucket} drill-down page
// (#598). Every field is a pre-formatted, template-safe string or plain value;
// the handler does all formatting so the template stays logic-free.
type QueueView struct {
	// Key is the bucket key used in URLs (e.g. "unavailable"); Title and Blurb
	// are the page heading and its one-line explanation.
	Key   string
	Title string
	Blurb string
	// Rows is one page of rows, already in id order.
	Rows []QueueRow
	// NextCursor is the keyset cursor for "Show more" (the last row's ID), or
	// zero when the bucket is exhausted. After is the cursor this page started
	// from; non-zero means the page does not begin at the top of the bucket.
	NextCursor int64
	After      int64
	// ReviveLink shows the "Revive retired tracks" link (the unavailable bucket
	// only, and only when a queue action backend is wired).
	ReviveLink bool
}

// ReviveView is the presentation model for /queue/unavailable/revive (#598): the
// blast radius of reviving retired tracks, the library picker, and (after a
// confirm) the result.
type ReviveView struct {
	// Manageable is false when no CSRF token could be issued; the page then
	// renders the numbers but no form, mirroring the keys page's fail-safe.
	Manageable bool
	CSRFToken  string
	Error      string

	// Total, Shared and Unlinked describe the all-libraries population.
	Total    int64
	Shared   int64
	Unlinked int64

	Libraries []ReviveLibrary
	// Selected is "all" or a library id; ScopeLabel/ScopeCount/ScopeShared
	// describe exactly what the confirm button will revive.
	Selected    string
	ScopeLabel  string
	ScopeCount  int64
	ScopeShared int64

	// Result is set on the page rendered after a confirm.
	Result *ReviveResult
}

// ReviveLibrary is one library option in the picker.
type ReviveLibrary struct {
	ID     string
	Name   string
	Count  int64
	Shared int64
}

// ReviveResult reports a completed revive.
type ReviveResult struct {
	Revived int64
	Scope   string
}

// itoa64 formats a count for templates.
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// QueueRow is one work_queue row as the drill-down table shows it.
type QueueRow struct {
	Artist        string
	Title         string
	Album         string
	Status        string
	Reason        string
	NextAttemptAt string
	MissCount     string
	Attempts      string
	UpdatedAt     string
	// Libraries is the comma-joined names of every library the row is linked to
	// ("-" when none).
	Libraries string
	// PreviewHref is the /preview/{id} player link, set only for a row with a
	// synced .lrc; empty renders no link.
	PreviewHref string
}

// QueueMoreHref is the "Show more" target for a bucket and cursor.
func QueueMoreHref(key string, cursor int64) string {
	return "/queue/" + key + "?after=" + strconv.FormatInt(cursor, 10)
}
