package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	RandomBytes      = 32
	PrefixDisplayLen = 4
	MaxNameLen       = 128
	DefaultName      = "Unnamed Key"
)

// Generate returns prefix + base64url random secret.
func Generate(prefix string) (string, error) {
	buf := make([]byte, RandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func LooksLike(key, prefix string) bool {
	return strings.HasPrefix(key, prefix)
}

// DisplayPrefix is the wire prefix plus a short secret preview for UI.
func DisplayPrefix(key, prefix string) string {
	n := len(prefix) + PrefixDisplayLen
	if len(key) <= n {
		return key
	}
	return key[:n]
}

// NormalizeName trims name, applies default, and enforces max length.
func NormalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		name = DefaultName
	}
	if len(name) > MaxNameLen {
		return "", fmt.Errorf("name too long")
	}
	return name, nil
}
