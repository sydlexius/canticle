package web

import (
	"strings"
	"testing"
)

// rowHTML returns the <tr> markup of the row titled title.
func rowHTML(t *testing.T, body, title string) string {
	t.Helper()
	for _, seg := range strings.Split(body, "<tr") {
		if strings.Contains(seg, ">"+title+"<") {
			return seg
		}
	}
	t.Fatalf("no row titled %q", title)
	return ""
}

// TestQueueEditedBadgeAndPreviewLabel pins #1236: the Edited badge follows
// lyric_edited_at (with the saved offset as its title) and the link text
// follows line-editability (a word-synced row stays "Preview").
func TestQueueEditedBadgeAndPreviewLabel(t *testing.T) {
	db := openReportsTestDB(t)
	seedChipPage(t, db)
	if _, err := db.Exec(`UPDATE work_queue SET lyric_offset_ms = 600 WHERE title = 'Done 002'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE work_queue SET lyric_offset_ms = -1234, lyric_edited_at = '2026-01-01T00:00:00Z' WHERE title = 'Done 001'`); err != nil {
		t.Fatal(err)
	}
	mux := newReportsUIServer(t, db)
	settled := getQueue(t, mux, "/queue/settled", false).Body.String()
	finished := getQueue(t, mux, "/queue/finished", false).Body.String()

	for _, tc := range []struct {
		body, title string
		badge       bool
		label       string
	}{
		{settled, "Done 001", true, "Preview / edit timing"},
		{settled, "Done 002", true, "Preview / edit timing"},
		{finished, "Done 003", false, ">Preview<"},
		{finished, "Done 004", true, ">Preview<"},
	} {
		row := rowHTML(t, tc.body, tc.title)
		if got := strings.Contains(row, "mx-queue-edited"); got != tc.badge {
			t.Errorf("%s: badge = %v, want %v", tc.title, got, tc.badge)
		}
		if !strings.Contains(row, tc.label) {
			t.Errorf("%s: link text want %q in %s", tc.title, tc.label, row)
		}
		if tc.label == ">Preview<" && strings.Contains(row, "edit timing") {
			t.Errorf("%s: word-synced row offers edit timing", tc.title)
		}
	}
	if row := rowHTML(t, settled, "Done 002"); !strings.Contains(row, ">Edited +0.60 s<") {
		t.Errorf("Done 002 badge lacks the visible saved offset: %s", row)
	}
	if row := rowHTML(t, settled, "Done 001"); !strings.Contains(row, ">Edited -1.23 s<") {
		t.Errorf("Done 001 badge lacks the visible negative offset: %s", row)
	}
}

// An edited row with no preview link (plain text here: nothing to preview)
// still carries the badge, alone in its cell, with no link beside it.
func TestQueueEditedBadgeWithoutPreviewLink(t *testing.T) {
	db := openReportsTestDB(t)
	seedChipPage(t, db)
	if _, err := db.Exec(`UPDATE work_queue SET lyric_offset_ms = 250, lyric_edited_at = '2026-01-01T00:00:00Z' WHERE title = 'Done 006'`); err != nil {
		t.Fatal(err)
	}
	row := rowHTML(t, getQueue(t, newReportsUIServer(t, db), "/queue/settled", false).Body.String(), "Done 006")
	if !strings.Contains(row, ">Edited +0.25 s<") {
		t.Errorf("edited row without a preview link lacks the badge: %s", row)
	}
	if strings.Contains(row, "mx-queue-preview") {
		t.Errorf("plain-text row offers a preview link: %s", row)
	}
}

func TestFormatEditOffset(t *testing.T) {
	for ms, want := range map[int64]string{-4: "0.00 s", 4: "0.00 s", 0: "0.00 s", -5: "-0.01 s", 600000: "+600.00 s", -1234: "-1.23 s"} {
		if got := formatEditOffset(ms); got != want {
			t.Errorf("formatEditOffset(%d) = %q, want %q", ms, got, want)
		}
	}
}
