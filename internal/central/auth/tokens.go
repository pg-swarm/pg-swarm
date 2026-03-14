package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
)

// GenerateToken creates a cryptographically random token string (32 bytes, hex-encoded = 64 chars)
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HashToken returns the SHA-256 hash of a token (hex-encoded)
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// ValidateToken checks if a raw token matches a stored hash
func ValidateToken(token, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashToken(token)), []byte(hash)) == 1
}
