package secrets

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// DefaultKeyFileName is the hidden key file written alongside the database on
// native installs. Its directory is resolved by the caller (the XDG data dir).
const DefaultKeyFileName = ".mxlrcgo.key"

// KeyOptions describes how to resolve the 32-byte master key. The caller (the
// config/startup layer) supplies the already-resolved inputs so this package
// stays decoupled from config and easily testable: MasterKeyB64 is the raw
// MXLRC_MASTER_KEY env value (optional override), and KeyFilePath is the
// resolved key file location (default DefaultKeyFileName under the data dir,
// or a secrets.key_file / MXLRC_SECRETS_KEY_FILE override).
type KeyOptions struct {
	MasterKeyB64 string
	KeyFilePath  string
	Logger       *slog.Logger
}

func (o KeyOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// ResolveKey returns the 32-byte AES-256 master key.
//
// Resolution order (first present wins):
//  1. MXLRC_MASTER_KEY (MasterKeyB64): base64 of exactly 32 bytes. A malformed
//     value (bad base64 or wrong length) is a loud, fatal error - never a
//     silent fallback to no encryption.
//  2. Key file (KeyFilePath): read it (warning on loose perms), or auto-generate
//     a 0600 file on first use. This is the universal zero-setup default on all
//     platforms including Docker. An unreadable/unwritable/malformed key file is
//     a loud, fatal error.
//
// MXLRC_MASTER_KEY and MXLRC_SECRETS_KEY_FILE are optional overrides: set
// MXLRC_MASTER_KEY to keep the key off the data volume (recommended when the
// key file and DB share the same bind-mount), or point MXLRC_SECRETS_KEY_FILE
// at a separately-mounted path for key/data separation without an env var.
func ResolveKey(opts KeyOptions) ([]byte, error) {
	if opts.MasterKeyB64 != "" {
		key, err := decodeMasterKey(opts.MasterKeyB64)
		if err != nil {
			return nil, err
		}
		return key, nil
	}

	return loadOrCreateKeyFile(opts)
}

// decodeMasterKey decodes a base64-encoded 32-byte key, failing loudly on bad
// encoding or wrong length.
func decodeMasterKey(b64 string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("secrets: MXLRC_MASTER_KEY is not valid base64: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets: MXLRC_MASTER_KEY must decode to %d bytes, got %d", KeySize, len(key))
	}
	return key, nil
}

// loadOrCreateKeyFile reads the 32-byte key file, or creates it 0600 on first
// use. Loose permissions on an existing file warn (non-fatal); any other I/O or
// length problem is fatal.
func loadOrCreateKeyFile(opts KeyOptions) ([]byte, error) {
	if opts.KeyFilePath == "" {
		return nil, errors.New("secrets: key file path must not be empty")
	}

	info, err := os.Stat(opts.KeyFilePath)
	switch {
	case err == nil:
		if info.Mode().Perm()&0o077 != 0 {
			opts.logger().Warn("secrets: key file has loose permissions",
				"path", opts.KeyFilePath, "mode", info.Mode().Perm().String())
		}
		key, err := os.ReadFile(opts.KeyFilePath) //nolint:gosec // path is operator-configured, not attacker-controlled
		if err != nil {
			return nil, fmt.Errorf("secrets: read key file %s: %w", opts.KeyFilePath, err)
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("secrets: key file %s must contain %d bytes, got %d", opts.KeyFilePath, KeySize, len(key))
		}
		return key, nil
	case errors.Is(err, os.ErrNotExist):
		return createKeyFile(opts.KeyFilePath)
	default:
		return nil, fmt.Errorf("secrets: stat key file %s: %w", opts.KeyFilePath, err)
	}
}

// createKeyFile generates a fresh 32-byte key and writes it 0600 via an
// atomic exclusive create so that concurrent first-start races are safe: the
// loser detects os.ErrExist and falls through to read the winner's key, so
// both callers end up with the same key.
func createKeyFile(path string) ([]byte, error) {
	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path is operator-configured, not attacker-controlled
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Lost the race: another process won; read its key.
			existing, readErr := os.ReadFile(path) //nolint:gosec // path is operator-configured, not attacker-controlled
			if readErr != nil {
				return nil, fmt.Errorf("secrets: read key file %s after lost-race create: %w", path, readErr)
			}
			if len(existing) != KeySize {
				return nil, fmt.Errorf("secrets: key file %s must contain %d bytes, got %d", path, KeySize, len(existing))
			}
			return existing, nil
		}
		return nil, fmt.Errorf("secrets: write key file %s: %w", path, err)
	}
	_, writeErr := f.Write(key)
	closeErr := f.Close()
	if writeErr != nil {
		return nil, fmt.Errorf("secrets: write key file %s: %w", path, writeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("secrets: close key file %s: %w", path, closeErr)
	}
	return key, nil
}
