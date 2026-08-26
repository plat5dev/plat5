package apikey

import (
	"regexp"
	"strings"
)

const (
	MaxScopeCount = 32
	MaxScopeLen   = 64
)

var scopeLabelRe = regexp.MustCompile(`^[a-z0-9:._-]+$`)

// ScopeError is a mint-time scopes validation failure.
// Message is product copy (error-copy.md).
type ScopeError struct {
	Message string
}

func (e *ScopeError) Error() string {
	return e.Message
}

var (
	ErrScopeInvalid   = &ScopeError{Message: "That scope label isn't valid."}
	ErrScopeTooLong   = &ScopeError{Message: "That scope label is too long."}
	ErrScopeTooMany   = &ScopeError{Message: "Too many scopes."}
	ErrScopeDuplicate = &ScopeError{Message: "Scope labels must be unique."}
)

// NormalizeScopes maps mint input to a stored list.
// nil / omitted → unrestricted (nil). Empty slice → grants nothing.
func NormalizeScopes(raw *[]string) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	in := *raw
	if len(in) > MaxScopeCount {
		return nil, ErrScopeTooMany
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, rawLabel := range in {
		s := strings.TrimSpace(rawLabel)
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

// WireScopes is nil when unrestricted so JSON encodes null (not omitted).
func WireScopes(scopes []string) *[]string {
	if scopes == nil {
		return nil
	}
	cp := scopes
	return &cp
}

// WireScopesJSON is for fiber.Map so unrestricted is JSON null, not omitted.
func WireScopesJSON(scopes []string) any {
	if scopes == nil {
		return nil
	}
	return scopes
}
