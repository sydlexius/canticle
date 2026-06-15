// Package webauth owns browser-based authentication for the serve-mode web UI:
// Argon2id password hashing, an admin user store, a server-side session store
// (raw tokens are hashed at rest), and a Service tying them together. It is kept
// separate from internal/auth (the stateless, in-memory API-key path) because the
// two have different storage, lifecycle, and threat models.
//
// This file is lane 1 of issue #204 (see docs/design/2026-06-14-204-auth-web-tls.md):
// the storage and service core only. No HTTP handlers, middleware, or server
// wiring live here; those land in later lanes.
package webauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP-recommended baseline; sign-off item S1 chose
// Argon2id over PBKDF2 reuse and bcrypt). They are encoded into every PHC string
// so the parameters and salt travel with the hash and can be tuned later without
// a migration. memory is in KiB.
const (
	argonMemory  uint32 = 64 * 1024 // 64 MiB
	argonTime    uint32 = 1
	argonThreads uint8  = 4
	argonSaltLen        = 16
	argonKeyLen  uint32 = 32
)

// Safe upper bounds on decoded Argon2id parameters. A hostile or corrupt PHC
// string with extreme values would exhaust memory or CPU before verify returns.
const (
	maxArgonMemoryKiB uint32 = 8 * 1024 * 1024 // 8 GiB
	maxArgonTime      uint32 = 100
	maxArgonThreads   uint8  = 64
)

var (
	// ErrInvalidHash is returned when an encoded hash is not a well-formed
	// Argon2id PHC string.
	ErrInvalidHash = errors.New("webauth: invalid password hash format")
	// ErrIncompatibleVersion is returned when the encoded hash was produced by a
	// different Argon2 version than this build understands.
	ErrIncompatibleVersion = errors.New("webauth: incompatible argon2 version")
)

// dummyHash is a syntactically valid Argon2id PHC string over a zero salt and
// key. It is used by the Service to run a verify against a non-existent user so
// the missing-user and wrong-password paths cost the same time (no username
// enumeration oracle). Only its m/t/p fields drive the recompute cost, so a
// constant value is sufficient and cannot fail to construct.
var dummyHash = encodeHash(make([]byte, argonSaltLen), make([]byte, int(argonKeyLen)))

// HashPassword returns an Argon2id PHC-encoded hash of password using a fresh
// 16-byte random salt and the package parameters.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("webauth: generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return encodeHash(salt, key), nil
}

// VerifyPassword reports whether password matches the Argon2id PHC-encoded hash.
// The hash compare is constant-time. A malformed encoded hash returns an error
// (ErrInvalidHash / ErrIncompatibleVersion), not a false negative, so callers can
// distinguish a wrong password from a corrupt record.
func VerifyPassword(encoded, password string) (bool, error) {
	memory, time, threads, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	if memory > maxArgonMemoryKiB || time > maxArgonTime || threads > maxArgonThreads {
		return false, fmt.Errorf("webauth: argon2 parameters exceed safe bounds")
	}
	//nolint:gosec // reason: key length is bounded by decodeHash (non-empty, base64-decoded from our own PHC string whose key is argonKeyLen=32 bytes); no int->uint32 overflow is possible.
	computed := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(key)))
	return subtle.ConstantTimeCompare(computed, key) == 1, nil
}

// encodeHash renders salt and key as a standard Argon2id PHC string:
// $argon2id$v=19$m=<KiB>,t=<iters>,p=<lanes>$<b64salt>$<b64key>. The base64 is
// raw (unpadded) standard encoding, per the PHC convention.
func encodeHash(salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// decodeHash parses an Argon2id PHC string into its parameters, salt, and key.
func decodeHash(encoded string) (memory, time uint32, threads uint8, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	// Leading "$" yields an empty first element: ["", "argon2id", "v=..", "m=..", salt, key].
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return 0, 0, 0, nil, nil, ErrIncompatibleVersion
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}

	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	if len(salt) == 0 || len(key) == 0 {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	return memory, time, threads, salt, key, nil
}
