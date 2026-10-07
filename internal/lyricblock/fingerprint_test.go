package lyricblock

import (
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

func TestFingerprint_SameWordsSameHash(t *testing.T) {
	synced := "[ar:Invented Artist]\n[00:01.00]Zorp the Blent, quilvane\n[00:05.50]Frabble on, dunsy-moor!\n[00:09.00]♪\n"
	want := Fingerprint(synced)
	if want == "" {
		t.Fatal("expected a fingerprint")
	}
	cases := map[string]string{
		"unsynced":   "Zorp the Blent, quilvane\nFrabble on, dunsy-moor!\n",
		"retimed":    "[01:11.20]Zorp the Blent, quilvane\n[01:15.00]Frabble on, dunsy-moor!\n",
		"wordtimed":  "[00:01.00]<00:01.00>Zorp <00:01.40>the Blent, quilvane\n[00:05.50]<00:05.50>Frabble on, dunsy-moor!\n",
		"reflowed":   "ZORP the blent\nquilvane frabble\n\non DUNSY moor\n",
		"multistamp": "[00:01.00][02:01.00]Zorp the Blent, quilvane\n[00:05.50]Frabble on, dunsy-moor!\n",
		"crlf":       "Zorp the Blent, quilvane\r\nFrabble on, dunsy-moor!\r\n",
	}
	for name, body := range cases {
		if got := Fingerprint(body); got != want {
			t.Errorf("%s: fingerprint %q != %q", name, got, want)
		}
	}
}

func TestFingerprint_DifferentBodyDiffers(t *testing.T) {
	a := Fingerprint("Zorp the Blent\nFrabble on")
	b := Fingerprint("Zorp the Blent\nFrabble off")
	if a == "" || b == "" || a == b {
		t.Fatalf("expected two distinct fingerprints, got %q and %q", a, b)
	}
}

func TestFingerprint_NoWordsNoFingerprint(t *testing.T) {
	for name, body := range map[string]string{
		"empty":        "",
		"whitespace":   " \n\n",
		"instrumental": "[00:00.00]♪ Instrumental ♪\n",
		"tags only":    "[ar:Invented]\n[ti:Nothing]\n",
		"punctuation":  "... --- !!!",
	} {
		if got := Fingerprint(body); got != "" {
			t.Errorf("%s: want no fingerprint, got %q", name, got)
		}
	}
}

func TestFingerprintSong_MatchesOnDiskBody(t *testing.T) {
	body := "[00:01.00]Zorp the Blent, quilvane\n[00:05.50]Frabble on\n"
	synced := models.Song{Subtitles: models.Synced{Lines: []models.Lines{
		{Text: "Zorp the Blent, quilvane"}, {Text: "Frabble on"}, {Text: "♪"},
	}}}
	plain := models.Song{Lyrics: models.Lyrics{LyricsBody: "Zorp the Blent, quilvane\nFrabble on\n"}}
	want := Fingerprint(body)
	if got := FingerprintSong(synced); got != want {
		t.Errorf("synced song = %q, want %q", got, want)
	}
	if got := FingerprintSong(plain); got != want {
		t.Errorf("plain song = %q, want %q", got, want)
	}
	if got := FingerprintSong(models.Song{}); got != "" {
		t.Errorf("empty song = %q, want none", got)
	}
}
