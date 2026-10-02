package web

import (
	"strings"
	"testing"
)

func TestPreviewPageNamesTheAudioFormat(t *testing.T) {
	for _, tc := range []struct{ file, format, ctype string }{
		{"song.m4a", "M4A", "audio/mp4"},
		{"song.WMA", "WMA", "audio/x-ms-wma"},
		// An extension outside previewAudioTypes is still named, and served as
		// an opaque stream the browser will refuse; the page must say so (#1243).
		{"song.ape", "APE", "application/octet-stream"},
	} {
		f := newPreviewFixture(t)
		id := f.row(t, f.writeFile(t, f.root, tc.file))
		f.put(t, "song.lrc", pageLRC)
		body := f.page(itoa(id)).Body.String()
		for _, want := range []string{
			`data-format="` + tc.format + `"`,
			`data-type="` + tc.ctype + `"`,
			`id="mx-preview-audio-error"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: body missing %q", tc.file, want)
			}
		}
	}
}

func TestPreviewFormat(t *testing.T) {
	for in, want := range map[string]string{"/m/a.m4a": "M4A", "/m/a.FlAc": "FLAC", "/m/noext": "", "": ""} {
		if got := previewFormat(in); got != want {
			t.Errorf("previewFormat(%q) = %q, want %q", in, got, want)
		}
	}
}
