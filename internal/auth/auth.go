// Package auth generates and verifies gateway virtual keys and compares
// secrets in constant time. Plaintext keys are returned to the caller exactly
// once at creation; only SHA-256 hashes are ever stored.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// KeyPrefix marks gateway-issued virtual keys.
const KeyPrefix = "sk-gw-"

// GenerateKey returns a new plaintext virtual key and its SHA-256 hex hash.
func GenerateKey() (plaintext, hash string, err error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", fmt.Errorf("generate key material: %w", err)
	}
	plaintext = KeyPrefix + hex.EncodeToString(raw[:])
	return plaintext, HashKey(plaintext), nil
}

// HashKey returns the SHA-256 hex digest of a plaintext key.
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// LooksLikeKey reports whether s carries the gateway key prefix.
func LooksLikeKey(s string) bool {
	return strings.HasPrefix(s, KeyPrefix)
}

// Equal compares two secrets in constant time, independent of their lengths.
func Equal(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
