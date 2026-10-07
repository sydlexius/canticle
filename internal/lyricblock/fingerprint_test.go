package lyricblock

import (
	"slices"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

func sameHash(t *testing.T, want string, bodies map[string]string) {
	t.Helper()
	for name, body := range bodies {
		if got := Fingerprint(body); got != want || got == "" {
			t.Errorf("%s: fingerprint %q != %q", name, got, want)
		}
	}
}

func TestFingerprint_SameWordsSameHash(t *testing.T) {
	want := Fingerprint("[ar:Invented Artist]\n[00:01.00]Zorp the Blent, quilvane\n[00:05.50]Frabble on, dunsy-moor!\n[00:09.00]♪\n")
	sameHash(t, want, map[string]string{
		"unsynced":   "Zorp the Blent, quilvane\nFrabble on, dunsy-moor!\n",
		"retimed":    "[01:11.20]Zorp the Blent, quilvane\n[01:15.00]Frabble on, dunsy-moor!\n",
		"wordtimed":  "[00:01.00]<00:01.00>Zorp <00:01.40>the Blent, quilvane\n[00:05.50]<00:05.50>Frabble on, dunsy-moor!\n",
		"reflowed":   "ZORP the blent\nquilvane frabble\n\non DUNSY moor\n",
		"crlf":       "Zorp the Blent, quilvane\r\nFrabble on, dunsy-moor!\r\n",
		"bom":        "\uFEFF[00:01.00]Zorp the Blent, quilvane\n[00:05.50]Frabble on, dunsy-moor!\n",
		"other tags": "[upstream:x]\n[re:canticle]\n[ve:1.2.3]\nZorp the Blent, quilvane\nFrabble on, dunsy-moor!\n",
	})
}

// A compressed multi-stamp chorus hashes as the chorus repeated: the same as
// its expanded form and an unsynced copy that repeats it, not as it once.
func TestFingerprint_CompressedRepeatedChorus(t *testing.T) {
	want := Fingerprint("[00:01.00]Zorp the Blent\n[00:05.00]Frabble on\n[00:09.00]Zorp the Blent\n")
	sameHash(t, want, map[string]string{
		"compressed": "[00:01.00][00:09.00]Zorp the Blent\n[00:05.00]Frabble on\n",
		"unsynced":   "Zorp the Blent\nFrabble on\nZorp the Blent\n",
	})
	if Fingerprint("Zorp the Blent\nFrabble on\n") == want {
		t.Error("chorus once hashes equal to chorus twice")
	}
}

func TestFingerprint_UnspacedScriptIgnoresSegmentation(t *testing.T) {
	want := Fingerprint("甲乙丙丁戊己庚辛")
	sameHash(t, want, map[string]string{
		"comma":      "甲乙丙丁，戊己庚辛",
		"line break": "甲乙丙\n丁戊己庚辛",
		"two cues":   "[00:01.00]甲乙丙丁\n[00:05.00]戊己庚辛\n",
	})
}

func TestFingerprint_DifferentBodyDiffersAndNoWordsHasNone(t *testing.T) {
	a := Fingerprint("Zorp the Blent\nFrabble on")
	for _, body := range []string{"Zorp the Blent\nFrabble off", "[Chorus]\nZorp the Blent\nFrabble on"} {
		if got := Fingerprint(body); got == a || got == "" {
			t.Errorf("%q: fingerprint %q must differ from %q", body, got, a)
		}
	}
	for _, body := range []string{"", " \n\n", "[00:00.00]♪ Instrumental ♪\n", "[ar:Invented]\n[upstream:x]\n", "... --- !!!"} {
		if got := Fingerprint(body); got != "" {
			t.Errorf("%q: want no fingerprint, got %q", body, got)
		}
	}
}

func TestSongFingerprints(t *testing.T) {
	cue := func(sec int, text string) models.Lines {
		return models.Lines{Text: text, Time: models.MsToTime(sec * 1000)}
	}
	song := models.Song{
		Subtitles:            models.Synced{Lines: []models.Lines{cue(1, "Zorp the Blent"), cue(5, "Frabble on")}},
		TranslationSubtitles: models.Synced{Lines: []models.Lines{cue(1, "Quix vorl"), cue(9, "orphan")}},
		Lyrics:               models.Lyrics{LyricsBody: "Wholly different body"},
	}
	want := []string{
		Fingerprint("[00:01.00]Zorp the Blent\n[00:05.00]Frabble on\n"),
		Fingerprint("[00:01.00]Zorp the Blent\n[00:01.00]Quix vorl\n[00:05.00]Frabble on\n"),
		Fingerprint("Wholly different body"),
	}
	if got := SongFingerprints(song); !slices.Equal(got, want) {
		t.Errorf("SongFingerprints = %v, want %v", got, want)
	}
	song.TranslationSubtitles, song.Lyrics.LyricsBody = models.Synced{}, "Zorp the Blent\nFrabble on"
	if got := SongFingerprints(song); len(got) != 1 {
		t.Errorf("equal cue and body streams = %v, want one", got)
	}
	song.Track.Instrumental = 1
	if got, empty := SongFingerprints(song), SongFingerprints(models.Song{}); got != nil || empty != nil {
		t.Errorf("instrumental = %v, empty = %v; want none", got, empty)
	}
}
