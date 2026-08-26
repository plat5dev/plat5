package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const (
	RandomBytes      = 32
	PrefixDisplayLen = 4
	MaxNameLen       = 128
	DefaultName      = "Unnamed Key"

	MaxScopeCount = 32
	MaxScopeLen   = 64
)

var scopeLabelRe = regexp.MustCompile(`^[a-z0-9:._-]+$`)

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

// ScopeError is a validation failure for the scopes field.
type ScopeError struct {
	Message string
}

func (e *ScopeError) Error() string {
	return e.Message
}

var (
	ErrScopeTooMany   = &ScopeError{Message: "Too many scopes."}
	ErrScopeTooLong   = &ScopeError{Message: "That scope label is too long."}
	ErrScopeInvalid   = &ScopeError{Message: "That scope label isn't valid."}
	ErrScopeDuplicate = &ScopeError{Message: "Scope labels must be unique."}
)

// NormalizeScopes validates optional mint scopes.
// nil / omitted / JSON null → nil (unrestricted).
// Empty slice → empty slice (grants nothing).
func NormalizeScopes(raw *[]string) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	if len(*raw) > MaxScopeCount {
		return nil, ErrScopeTooMany
	}
	out := make([]string, 0, len(*raw))
	seen := make(map[string]struct{}, len(*raw))
	for _, s := range *raw {
		s = strings.TrimSpace(s)
		if s == "" || !scopeLabelRe.MatchString(s) {
			return nil, ErrScopeInvalid
		}
		if len(s) > MaxScopeLen {
			return nil, ErrScopeTooLong
		}
		if _, ok := seen[s]; ok {
			return nil, ErrScopeDuplicate
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

// WireScopes is JSON null when unrestricted (nil slice).
func WireScopes(scopes []string) *[]string {
	if scopes == nil {
		return nil
	}
	s := scopes
	return &s
}

// WireScopesJSON is null when unrestricted, otherwise a JSON array.
func WireScopesJSON(scopes []string) any {
	if scopes == nil {
		return nil
	}
	return scopes
}
