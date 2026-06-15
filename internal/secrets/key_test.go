package secrets

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveKeyEnvWinsOverFile(t *testing.T) {
	// Write a key file that should be ignored when MXLRC_MASTER_KEY is set.
	dir := t.TempDir()
	keyPath := filepath.Join(dir, DefaultKeyFileName)
	fileKey := bytes.Repeat([]byte{0xAA}, KeySize)
	if err := os.WriteFile(keyPath, fileKey, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	envKey := bytes.Repeat([]byte{0xBB}, KeySize)
	got, err := ResolveKey(KeyOptions{
		MasterKeyB64: base64.StdEncoding.EncodeToString(envKey),
		KeyFilePath:  keyPath,
	})
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if !bytes.Equal(got, envKey) {
		t.Fatal("env key did not win over key file")
	}
}

func TestResolveKeyMalformedMasterKeyFatal(t *testing.T) {
	cases := map[string]string{
		"bad base64":   "not!valid!base64!",
		"wrong length": base64.StdEncoding.EncodeToString([]byte("only-a-few-bytes")),
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveKey(KeyOptions{MasterKeyB64: val}); err == nil {
				t.Fatal("ResolveKey accepted malformed MXLRC_MASTER_KEY; want fatal error")
			}
		})
	}
}

func TestResolveKeyAutoCreatesKeyFile0600(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, DefaultKeyFileName)
	got, err := ResolveKey(KeyOptions{KeyFilePath: keyPath})
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if len(got) != KeySize {
		t.Fatalf("generated key len = %d, want %d", len(got), KeySize)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", info.Mode().Perm())
	}
	// A second resolve reads the same key back.
	again, err := ResolveKey(KeyOptions{KeyFilePath: keyPath})
	if err != nil {
		t.Fatalf("ResolveKey (reload): %v", err)
	}
	if !bytes.Equal(got, again) {
		t.Fatal("reloaded key differs from generated key")
	}
}

func TestResolveKeyLoosePermsWarns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits unreliable on Windows")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, DefaultKeyFileName)
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x11}, KeySize), 0o644); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if _, err := ResolveKey(KeyOptions{KeyFilePath: keyPath, Logger: logger}); err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if !strings.Contains(buf.String(), "loose permissions") {
		t.Fatalf("expected loose-permissions warning, got: %q", buf.String())
	}
}

func TestResolveKeyBadLengthFileFatal(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, DefaultKeyFileName)
	if err := os.WriteFile(keyPath, []byte("too short"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	if _, err := ResolveKey(KeyOptions{KeyFilePath: keyPath}); err == nil {
		t.Fatal("ResolveKey accepted wrong-length key file; want fatal error")
	}
}

func TestResolveKeyEmptyKeyFilePathFatal(t *testing.T) {
	if _, err := ResolveKey(KeyOptions{}); err == nil {
		t.Fatal("ResolveKey with no master key and no key file path succeeded; want error")
	}
}

// TestCreateKeyFileRaceNoOverwrite verifies the concurrent-startup safe path: if
// the key file already exists when createKeyFile is called (i.e. another process
// won the race), the loser reads and returns the existing key without overwriting
// it, and the file content is unchanged.
func TestCreateKeyFileRaceNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, DefaultKeyFileName)

	// Pre-create the file with a known key (simulates the winner's write).
	existing := bytes.Repeat([]byte{0xCC}, KeySize)
	if err := os.WriteFile(keyPath, existing, 0o600); err != nil {
		t.Fatalf("pre-create key file: %v", err)
	}

	// The loser calls createKeyFile on the same path; it must not overwrite.
	got, err := createKeyFile(keyPath)
	if err != nil {
		t.Fatalf("createKeyFile on existing path: %v", err)
	}
	if !bytes.Equal(got, existing) {
		t.Fatalf("createKeyFile returned a different key: got %x, want %x", got, existing)
	}

	// File content must be unchanged.
	written, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key file after createKeyFile: %v", err)
	}
	if !bytes.Equal(written, existing) {
		t.Fatalf("key file content was modified: got %x, want %x", written, existing)
	}
}

// TestCreateKeyFileLostRaceWrongLength verifies that if the winner's key file
// contains a length other than KeySize, the loser returns a fatal error rather
// than silently returning a truncated or padded key.
func TestCreateKeyFileLostRaceWrongLength(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, DefaultKeyFileName)

	// Pre-create with wrong length - simulates a corrupt winner write.
	if err := os.WriteFile(keyPath, []byte("too short"), 0o600); err != nil {
		t.Fatalf("pre-create key file: %v", err)
	}

	_, err := createKeyFile(keyPath)
	if err == nil {
		t.Fatal("createKeyFile on wrong-length existing file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "must contain 32 bytes") {
		t.Fatalf("expected 'must contain 32 bytes' error, got: %v", err)
	}
}

// TestCreateKeyFileParentDirMissing verifies that a non-race OpenFile failure
// (parent directory missing, so ENOENT rather than EEXIST) is wrapped with
// the "write key file" message and propagated as a fatal error.
func TestCreateKeyFileParentDirMissing(t *testing.T) {
	dir := t.TempDir()
	// Path inside a non-existent subdirectory: OpenFile fails with ENOENT (not ErrExist).
	keyPath := filepath.Join(dir, "nonexistent-subdir", DefaultKeyFileName)

	_, err := createKeyFile(keyPath)
	if err == nil {
		t.Fatal("createKeyFile with missing parent dir: want error, got nil")
	}
	if !strings.Contains(err.Error(), "write key file") {
		t.Fatalf("expected 'write key file' error, got: %v", err)
	}
}

// TestCreateKeyFileLostRaceReadError verifies that if the winner's key file
// exists (O_EXCL returns ErrExist) but is not readable (e.g. mode 000),
// createKeyFile returns a fatal error naming the failed read rather than
// silently returning an empty or wrong key.
func TestCreateKeyFileLostRaceReadError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based read errors unreliable on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root; chmod 000 does not block reads")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, DefaultKeyFileName)

	// Pre-create with no-read permission: O_EXCL will return ErrExist, then
	// os.ReadFile fails with EACCES.
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0xAB}, KeySize), 0o000); err != nil {
		t.Fatalf("pre-create key file: %v", err)
	}
	// Restore before cleanup so t.TempDir can remove the directory.
	t.Cleanup(func() { _ = os.Chmod(keyPath, 0o600) })

	_, err := createKeyFile(keyPath)
	// Restore perms before asserting so the deferred cleanup always succeeds.
	if chmodErr := os.Chmod(keyPath, 0o600); chmodErr != nil {
		t.Logf("restore chmod: %v", chmodErr)
	}
	if err == nil {
		t.Fatal("createKeyFile with unreadable existing file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "after lost-race create") {
		t.Fatalf("expected 'after lost-race create' error, got: %v", err)
	}
}

// TestResolveKeyAutoCreatesKeyFileUniversal proves that with no env key and no
// existing key file, ResolveKey creates a 0600 key file and returns a valid
// 32-byte key, regardless of Docker-ish env state (MXLRC_DOCKER).
func TestResolveKeyAutoCreatesKeyFileUniversal(t *testing.T) {
	for _, docker := range []string{"", "true", "1"} {
		t.Run("MXLRC_DOCKER="+docker, func(t *testing.T) {
			t.Setenv("MXLRC_DOCKER", docker)
			dir := t.TempDir()
			keyPath := filepath.Join(dir, DefaultKeyFileName)
			got, err := ResolveKey(KeyOptions{KeyFilePath: keyPath})
			if err != nil {
				t.Fatalf("ResolveKey: %v", err)
			}
			if len(got) != KeySize {
				t.Fatalf("key len = %d, want %d", len(got), KeySize)
			}
			info, statErr := os.Stat(keyPath)
			if statErr != nil {
				t.Fatalf("stat key file: %v", statErr)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Fatalf("key file mode = %v, want 0600", info.Mode().Perm())
			}
		})
	}
}
