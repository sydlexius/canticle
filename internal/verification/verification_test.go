package verification

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

func TestSongTextPrefersSubtitles(t *testing.T) {
	song := models.Song{
		Lyrics: models.Lyrics{LyricsBody: "unsynced"},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "line one"},
			{Text: "line two"},
		}},
	}
	if got := SongText(song); got != "line one line two" {
		t.Fatalf("SongText = %q; want subtitle text", got)
	}
}

func TestSongTextFallsBackWhenSubtitleLinesBlank(t *testing.T) {
	song := models.Song{
		Lyrics: models.Lyrics{LyricsBody: "unsynced"},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: ""},
			{Text: "   "},
		}},
	}
	if got := SongText(song); got != "unsynced" {
		t.Fatalf("SongText = %q; want lyrics fallback", got)
	}
}

func TestSimilarityUsesTranscriptTokenCoverage(t *testing.T) {
	got := Similarity("hello bright world", "hello world this is the song")
	if got < 0.66 {
		t.Fatalf("Similarity = %v; want transcript token overlap", got)
	}
}

func TestSimilarityKeepsSingleCharacterTokens(t *testing.T) {
	got := Similarity("I am a", "I am a song")
	if got != 1 {
		t.Fatalf("Similarity = %v; want single-character tokens included", got)
	}
}

func TestHTTPVerifierVerifyPostsAudioAndComparesTranscript(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t, `printf 'sampled audio' > "$last"`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Fatalf("path = %q; want transcription endpoint", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		if got := r.FormValue("sample_duration_seconds"); got != "" {
			t.Fatalf("sample_duration_seconds = %q; want no nonstandard field", got)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		b, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("read form file: %v", err)
		}
		if string(b) != "sampled audio" {
			t.Fatalf("uploaded audio = %q; want sampled audio", string(b))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "hello world"})
	}))
	defer srv.Close()

	v, err := NewHTTPVerifier(srv.URL, 45, 0.5, ffmpegPath)
	if err != nil {
		t.Fatalf("NewHTTPVerifier: %v", err)
	}
	if v.sampleDurationSeconds != 45 {
		t.Fatalf("sampleDurationSeconds = %d; want 45", v.sampleDurationSeconds)
	}
	res, err := v.Verify(context.Background(), audioPath, models.Song{
		Lyrics: models.Lyrics{LyricsBody: "hello world lyrics"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("accepted = false; want true")
	}
}

func TestHTTPVerifierBuildsFFmpegSampleCommand(t *testing.T) {
	got := ffmpegSampleArgs("song.flac", "sample.wav", 45)
	want := []string{
		"-nostdin",
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", "song.flac",
		"-t", "45",
		"-vn",
		"-ac", "1",
		"-ar", "16000",
		"sample.wav",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ffmpegSampleArgs = %#v; want %#v", got, want)
	}
}

func TestNewHTTPVerifierClampsSampleDuration(t *testing.T) {
	ffmpegPath := fakeFFmpeg(t, `printf 'sampled audio' > "$last"`)
	tests := []struct {
		name     string
		duration int
		want     int
	}{
		{name: "zero defaults to minimum", duration: 0, want: 30},
		{name: "below minimum", duration: 10, want: 30},
		{name: "within bounds", duration: 45, want: 45},
		{name: "above maximum", duration: 300, want: 60},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := NewHTTPVerifier("http://whisper:9000", tc.duration, 0.5, ffmpegPath)
			if err != nil {
				t.Fatalf("NewHTTPVerifier: %v", err)
			}
			if v.sampleDurationSeconds != tc.want {
				t.Fatalf("sampleDurationSeconds = %d; want %d", v.sampleDurationSeconds, tc.want)
			}
		})
	}
}

func TestNewHTTPVerifierErrorsWhenFFmpegMissing(t *testing.T) {
	_, err := NewHTTPVerifier("http://whisper:9000", 30, 0.5, filepath.Join(t.TempDir(), "missing-ffmpeg"))
	if err == nil {
		t.Fatal("NewHTTPVerifier returned nil error; want missing ffmpeg error")
	}
}

func TestNewHTTPVerifierRejectsSchemelesURL(t *testing.T) {
	for _, u := range []string{"/whisper", "whisper:9000", "example.com"} {
		_, err := NewHTTPVerifier(u, 30, 0.5, "ffmpeg")
		if err == nil {
			t.Errorf("NewHTTPVerifier(%q) returned nil error; want rejection of scheme-less URL", u)
		}
	}
}

func TestHTTPVerifierCleansSampleAfterFFmpegFailure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t, `printf 'partial sample' > "$last"; exit 2`)
	v, err := NewHTTPVerifier("http://whisper:9000", 30, 0.5, ffmpegPath)
	if err != nil {
		t.Fatalf("NewHTTPVerifier: %v", err)
	}

	_, err = v.Verify(context.Background(), audioPath, models.Song{
		Lyrics: models.Lyrics{LyricsBody: "hello world lyrics"},
	})
	if err == nil {
		t.Fatal("Verify returned nil error; want ffmpeg failure")
	}
	matches, err := filepath.Glob(filepath.Join(tmp, "mxlrcgo-svc-verify-*.wav"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("sample files after failure = %#v; want cleanup", matches)
	}
}

func TestHTTPVerifierCleansSampleAfterContextCancellation(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t, `printf 'sampled audio' > "$last"`)
	v, err := NewHTTPVerifier("http://whisper:9000", 30, 0.5, ffmpegPath)
	if err != nil {
		t.Fatalf("NewHTTPVerifier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = v.Verify(ctx, audioPath, models.Song{
		Lyrics: models.Lyrics{LyricsBody: "hello world lyrics"},
	})
	if err == nil {
		t.Fatal("Verify returned nil error; want context cancellation")
	}
	matches, err := filepath.Glob(filepath.Join(tmp, "mxlrcgo-svc-verify-*.wav"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("sample files after cancellation = %#v; want cleanup", matches)
	}
}

func TestHTTPVerifierTranscriptionURL(t *testing.T) {
	tests := map[string]string{
		"http://whisper:9000":                         "http://whisper:9000/v1/audio/transcriptions",
		"http://whisper:9000/":                        "http://whisper:9000/v1/audio/transcriptions",
		"http://whisper:9000/v1":                      "http://whisper:9000/v1/audio/transcriptions",
		"http://whisper:9000/v1/":                     "http://whisper:9000/v1/audio/transcriptions",
		"http://whisper:9000/v1/audio/transcriptions": "http://whisper:9000/v1/audio/transcriptions",
	}
	ffmpegPath := fakeFFmpeg(t, `printf 'sampled audio' > "$last"`)
	for rawURL, want := range tests {
		v, err := NewHTTPVerifier(rawURL, 30, 0.5, ffmpegPath)
		if err != nil {
			t.Fatalf("NewHTTPVerifier(%q): %v", rawURL, err)
		}
		if got := v.transcriptionURL(); got != want {
			t.Fatalf("transcriptionURL(%q) = %q; want %q", rawURL, got, want)
		}
	}
}

func fakeFFmpeg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\nlast=''\nfor arg do\n  last=\"$arg\"\ndone\n" + strings.TrimSpace(body) + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}
