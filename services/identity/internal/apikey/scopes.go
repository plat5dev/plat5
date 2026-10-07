package apikey

import (
	"errors"
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

// CallerScopesHeader is set by the gateway from the admitted credential.
// Absent means unrestricted. Present means restricted.
// Value is comma-separated labels, or "[]" when the list is empty.
const CallerScopesHeader = "X-Plat5-Scopes"

// ErrCallerScopes means the header is present but not a scope list.
// That is a platform bug (or a forged header the gateway should have replaced), not a client 422.
var ErrCallerScopes = errors.New("invalid caller scopes")

// InsufficientScopeError is a restricted caller asking for a label it does not have.
// Missing preserves request order.
type InsufficientScopeError struct {
	Missing []string
}

func (e *InsufficientScopeError) Error() string {
	return "insufficient scope"
}

// NormalizeScopes maps mint input to a stored list.
// nil / omitted → unrestricted (nil). Empty slice → restricted, no labels.
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

// ConstrainScopes is the mint rule for keys and sessions.
//
// caller nil: unrestricted. requested is stored as-is (nil stays unrestricted).
// caller non-nil: restricted, including an empty list.
// requested nil: inherit a copy of caller. The result is never nil.
// requested non-nil: every label must be in caller, or the error lists the ones that are not.
func ConstrainScopes(caller, requested []string) ([]string, error) {
	if caller == nil {
		return requested, nil
	}
	if requested == nil {
		return cloneScopes(caller), nil
	}
	if missing := missingScopes(caller, requested); len(missing) > 0 {
		return nil, &InsufficientScopeError{Missing: missing}
	}
	return requested, nil
}

// ErrTargetUnrestricted is a restricted caller acting on a key whose scopes are null.
var ErrTargetUnrestricted = errors.New("target credential is unrestricted")

// CheckWithin is the revoke rule: a restricted caller may only act on a key whose
// scopes fit within its own. Same subset check as the mint cap.
//
// caller nil: unrestricted, always allowed.
// target nil: unrestricted key, refused with ErrTargetUnrestricted.
// otherwise every target label must be in caller, or the error lists the ones that are not.
func CheckWithin(caller, target []string) error {
	if caller == nil {
		return nil
	}
	if target == nil {
		return ErrTargetUnrestricted
	}
	if missing := missingScopes(caller, target); len(missing) > 0 {
		return &InsufficientScopeError{Missing: missing}
	}
	return nil
}

// missingScopes returns the labels in want that caller does not have, in want order.
func missingScopes(caller, want []string) []string {
	have := make(map[string]struct{}, len(caller))
	for _, s := range caller {
		have[s] = struct{}{}
	}
	var missing []string
	for _, s := range want {
		if _, ok := have[s]; !ok {
			missing = append(missing, s)
		}
	}
	return missing
}

// ParseCallerScopes parses a present X-Plat5-Scopes value.
// Empty or "[]" is a restricted credential with no labels, not unrestricted.
func ParseCallerScopes(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return []string{}, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > MaxScopeCount {
		return nil, ErrCallerScopes
	}
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		s := strings.TrimSpace(part)
		if s == "" || len(s) > MaxScopeLen || !scopeLabelRe.MatchString(s) {
			return nil, ErrCallerScopes
		}
		if _, ok := seen[s]; ok {
			return nil, ErrCallerScopes
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

func cloneScopes(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
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
