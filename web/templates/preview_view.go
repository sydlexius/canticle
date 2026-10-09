package templates

import (
	"context"
	"html"
	"io"

	"github.com/a-h/templ"
)

// previewWords writes a line's word spans with the exact separators between
// them. It is Go rather than templ markup because templ collapses the
// whitespace between sibling nodes on separate lines into one space, which
// would put a space between two CJK words the source never separated.
func previewWords(l PreviewLine) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var err error
		put := func(s string) {
			if err == nil {
				_, err = io.WriteString(w, s)
			}
		}
		for _, word := range l.Words {
			put(html.EscapeString(word.Before))
			put(`<span class="mx-preview-word" data-start-ms="` + html.EscapeString(word.StartMS) + `">` + html.EscapeString(word.Text) + `</span>`)
		}
		return err
	})
}

// PreviewView is the view model for the preview player page (#481). The handler
// maps a reports.PreviewTarget and the parsed sidecars onto it, so the templ
// file stays free of lyrics-package concerns. It carries library content, so
// the page is session-guarded and no-store.
type PreviewView struct {
	Artist string
	Title  string
	Album  string
	// BackHref and BackLabel are the link back to the queue list the player was
	// opened from (the Preview link's ?from=<bucket>), or to /queue when the
	// origin is absent or not a known bucket.
	BackHref  string
	BackLabel string
	// AudioSrc is the same-origin audio route for the row.
	AudioSrc string
	// AudioFormat is the audio file's extension, upper-cased without the dot
	// ("M4A"), or "" when it has none; AudioType is the Content-Type the audio
	// route serves it as. The page names them when the browser cannot decode
	// the stream (#1243).
	AudioFormat string
	AudioType   string
	// FlacSrc is the FLAC fallback route the player retries once when the
	// browser cannot decode AudioSrc; "" when the fallback is off (#1243).
	FlacSrc string
	Lines   []PreviewLine
	// HasWords reports whether any line carries word timings (A2).
	HasWords bool
	// Truncated reports that the .lrc exceeded the read bound and the lines
	// are only its leading complete cues; the page says so.
	Truncated bool

	// NoLyric reports that no lyric file is written: the page has no lyric panel
	// (#1250). Marks is the mark-actions model (ID zero when the mark routes are
	// not wired, which renders no marks section) and MarkStatus the one-shot
	// result line of the action just taken.
	NoLyric    bool
	Marks      RowActions
	MarkStatus string

	// Lyric offset editor (#1211). Editable renders the editor panel (only for
	// a line-synced, settled row with the editor wired); a non-empty
	// ReadOnlyReason renders the read-only card instead.
	Editable       bool
	ReadOnlyReason string
	// EditURL is the row's /preview/{id} base; the panel posts to its
	// /offset and /revert children.
	EditURL string
	// AutoURL is the row's Auto alignment endpoint (#1008), rendered as
	// data-auto-url on the editor panel. Empty (attribute absent) unless an
	// aligner is attached, healthy, and the row is eligible.
	AutoURL    string
	OffsetMS   int
	Edited     bool
	MTime      string // the .lrc mtime in unix nanoseconds, decimal, as the page loaded it
	DurationMS int    // exact audio duration, 0 when unknown
	CSRFToken  string
	// OrigMS is the ORIGINAL start of each shown line, comma-separated in line
	// order: the base every offset is measured from (#1211).
	OrigMS string
	// ToleranceMS is timing.Tolerance in ms, so the client's past-end preview
	// uses the same allowance the server's timing guard enforces.
	ToleranceMS int
}

// PreviewLine is one lyric cue. StartMS is a decimal millisecond string, the
// value of the line's data-start-ms attribute.
type PreviewLine struct {
	StartMS    string
	Text       string
	Words      []PreviewWord
	Decorative bool
}

// PreviewWord is one A2 word with its own data-start-ms.
type PreviewWord struct {
	StartMS string
	// Before is the exact text preceding the word in its line (the separator,
	// or leading unmarked text for the first word); empty for CJK.
	Before string
	Text   string
}
