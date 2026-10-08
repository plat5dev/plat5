package apikey

import "regexp"

const MaxScopeLen = 64

var scopeLabelRe = regexp.MustCompile(`^[a-z0-9:._-]+$`)

// ScopesRefused is the 422 for a key mint that sends `scopes`. A key carries its
// owner's permissions. Refusing the field keeps an old client from believing it
// got a narrower key than it did.
const ScopesRefused = "Keys carry their owner's permissions and can't be narrowed."

// ValidLabel is the label hygiene shared by route required_scopes and roles.
func ValidLabel(s string) bool {
	return s != "" && len(s) <= MaxScopeLen && scopeLabelRe.MatchString(s)
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
