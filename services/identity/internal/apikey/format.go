package apikey

import "strings"

const (
	version   = "2"
	secretLen = 43 // RandomBytes, base64url without padding
)

// Prefix is the wire prefix identity mints: {brand}-k-2-
// (docs/reference/api/identity.md#credentials).
func Prefix(brand string) string {
	return brand + "-k-" + version + "-"
}

// Parse reads s as a key of this brand and returns its wire prefix. ok is false
// for any other shape or brand.
func Parse(brand, s string) (prefix string, ok bool) {
	prefix = Prefix(brand)
	secret, found := strings.CutPrefix(s, prefix)
	if !found || !validSecret(secret) {
		return "", false
	}
	return prefix, true
}

func validSecret(s string) bool {
	if len(s) != secretLen {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}
