package webauth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword(encoded, "correct horse battery staple")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("VerifyPassword returned false for the correct password")
	}
}

func TestVerifyPasswordWrongPassword(t *testing.T) {
	encoded, err := HashPassword("the right password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword(encoded, "the wrong password")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Fatal("VerifyPassword returned true for a wrong password")
	}
}

func TestHashPasswordEncodingFormat(t *testing.T) {
	encoded, err := HashPassword("hunter2hunter2")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	const wantPrefix = "$argon2id$v=19$m=65536,t=1,p=4$"
	if !strings.HasPrefix(encoded, wantPrefix) {
		t.Fatalf("hash prefix = %q, want prefix %q", encoded, wantPrefix)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		t.Fatalf("hash has %d $-parts, want 6: %q", len(parts), encoded)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}
	if len(salt) != argonSaltLen {
		t.Fatalf("salt length = %d, want %d", len(salt), argonSaltLen)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	if len(key) != int(argonKeyLen) {
		t.Fatalf("key length = %d, want %d", len(key), argonKeyLen)
	}
}

func TestHashPasswordUsesRandomSalt(t *testing.T) {
	a, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword a: %v", err)
	}
	b, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword b: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
}

func TestVerifyPasswordMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"not phc":          "plaintext",
		"wrong algorithm":  "$argon2i$v=19$m=65536,t=1,p=4$AAAA$AAAA",
		"too few fields":   "$argon2id$v=19$m=65536,t=1,p=4$AAAA",
		"bad params":       "$argon2id$v=19$m=foo,t=1,p=4$AAAA$AAAA",
		"bad salt base64":  "$argon2id$v=19$m=65536,t=1,p=4$!!!!$AAAA",
		"empty salt + key": "$argon2id$v=19$m=65536,t=1,p=4$$",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := VerifyPassword(encoded, "whatever")
			if ok {
				t.Fatal("VerifyPassword returned true for a malformed hash")
			}
			if !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("error = %v, want ErrInvalidHash", err)
			}
		})
	}
}

func TestVerifyPasswordOversizedParamsRejected(t *testing.T) {
	// A PHC string with m= exceeding the safe bound must be rejected before
	// argon2.IDKey is called; otherwise a hostile hash could exhaust memory.
	oversized := "$argon2id$v=19$m=9999999,t=1,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	ok, err := VerifyPassword(oversized, "any")
	if ok {
		t.Fatal("VerifyPassword returned true for an oversized-parameter hash")
	}
	if err == nil {
		t.Fatal("VerifyPassword returned nil error for an oversized-parameter hash")
	}
}

func TestVerifyPasswordIncompatibleVersion(t *testing.T) {
	// A PHC string with a version other than the one this build uses.
	encoded := "$argon2id$v=18$m=65536,t=1,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if argon2.Version == 18 {
		t.Skip("argon2.Version unexpectedly 18")
	}
	ok, err := VerifyPassword(encoded, "whatever")
	if ok {
		t.Fatal("VerifyPassword returned true for an incompatible version")
	}
	if !errors.Is(err, ErrIncompatibleVersion) {
		t.Fatalf("error = %v, want ErrIncompatibleVersion", err)
	}
}
