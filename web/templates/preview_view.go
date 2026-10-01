package templates

// PreviewView is the view model for the preview player page (#481). The handler
// maps a reports.PreviewTarget and the parsed sidecars onto it, so the templ
// file stays free of lyrics-package concerns. It carries library content, so
// the page is session-guarded and no-store.
type PreviewView struct {
	Artist string
	Title  string
	Album  string
	// AudioSrc is the same-origin audio route for the row.
	AudioSrc string
	Lines    []PreviewLine
	// HasWords reports whether any line carries word timings (A2).
	HasWords bool
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
	Text    string
}
