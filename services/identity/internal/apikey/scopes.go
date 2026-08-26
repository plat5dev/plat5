package apikey

import (
	"regexp"
	"strings"

	"github.com/plat5dev/plat5/identity/errors"
)

const (
	MaxScopeCount = 32
	MaxScopeLen   = 64
)

var scopeLabelRe = regexp.MustCompile(`^[a-z0-9:._-]+$`)

// ParseScopes maps mint input to a stored list.
// nil / omitted → unrestricted (nil). Empty slice → grants nothing.
func ParseScopes(raw *[]string) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	return NormalizeScopes(*raw)
}

// NormalizeScopes validates labels. Empty input returns an empty (non-nil) slice.
func NormalizeScopes(in []string) ([]string, error) {
	if len(in) > MaxScopeCount {
		return nil, errors.FieldError("scopes", "Too many scopes.")
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			return nil, errors.FieldError("scopes", "A scope label is required.")
		}
		if len(s) > MaxScopeLen {
			return nil, errors.FieldError("scopes", "A scope label is too long.")
		}
		if !scopeLabelRe.MatchString(s) {
			return nil, errors.FieldError("scopes", "Scopes can only use lowercase letters, numbers, colons, dots, underscores, and dashes.")
		}
		if _, ok := seen[s]; ok {
			return nil, errors.FieldError("scopes", "Duplicate scope labels are not allowed.")
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

// PointerForJSON is nil when unrestricted so JSON encodes null.
func PointerForJSON(scopes []string) *[]string {
	if scopes == nil {
		return nil
	}
	cp := scopes
	return &cp
}
