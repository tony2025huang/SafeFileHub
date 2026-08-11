// Package auth authenticates SafeFileHub users and manages opaque sessions.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

var ErrInvalidCredentials = errors.New("invalid credentials")

type argon2Params struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	saltLength  uint32
	keyLength   uint32
}

// These fixed parameters bound each login verification to 64 MiB and one
// worker. Accepting attacker-controlled encoded parameters would otherwise
// let a stored/corrupt hash exhaust the login concurrency memory budget.
var defaultArgon2 = argon2Params{memory: 64 * 1024, iterations: 3, parallelism: 1, saltLength: 16, keyLength: 32}

// HashPassword returns a self-describing Argon2id encoded hash. It never
// returns or stores the plaintext password.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("password must not be empty")
	}
	salt := make([]byte, defaultArgon2.saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, defaultArgon2.iterations, defaultArgon2.memory, defaultArgon2.parallelism, defaultArgon2.keyLength)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", defaultArgon2.memory, defaultArgon2.iterations, defaultArgon2.parallelism, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(encodedHash, password string) bool {
	params, salt, expected, ok := parseHash(encodedHash)
	if !ok {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, params.iterations, params.memory, params.parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func parseHash(encoded string) (argon2Params, []byte, []byte, bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return argon2Params{}, nil, nil, false
	}
	var p argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.iterations, &p.parallelism); err != nil || parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", defaultArgon2.memory, defaultArgon2.iterations, defaultArgon2.parallelism) {
		return argon2Params{}, nil, nil, false
	}
	// Bound input before decoding and require the generated fixed-size values.
	// This avoids allocating for arbitrarily long attacker/database input.
	if len(parts[4]) > base64.RawStdEncoding.EncodedLen(int(defaultArgon2.saltLength)) || len(parts[5]) > base64.RawStdEncoding.EncodedLen(int(defaultArgon2.keyLength)) {
		return argon2Params{}, nil, nil, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != int(defaultArgon2.saltLength) {
		return argon2Params{}, nil, nil, false
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(hash) != int(defaultArgon2.keyLength) {
		return argon2Params{}, nil, nil, false
	}
	return p, salt, hash, true
}
