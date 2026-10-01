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
}

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
}

// QueueMoreHref is the "Show more" target for a bucket and cursor.
func QueueMoreHref(key string, cursor int64) string {
	return "/queue/" + key + "?after=" + strconv.FormatInt(cursor, 10)
}
