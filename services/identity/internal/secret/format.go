package secret

import "strings"

// The kinds of secret identity mints, each with its format's version
// (docs/reference/api/identity.md#secrets).
const (
	keyKind       = "k"
	keyVersion    = "2"
	inviteKind    = "i"
	inviteVersion = "1"
	secretLen     = 43 // RandomBytes, base64url without padding
)

// KeyPrefix is the wire prefix of the API keys identity mints: {brand}-k-2-.
func KeyPrefix(brand string) string {
	return brand + "-" + keyKind + "-" + keyVersion + "-"
}

// InvitePrefix is the wire prefix of the invite tokens identity mints:
// {brand}-i-1-.
func InvitePrefix(brand string) string {
	return brand + "-" + inviteKind + "-" + inviteVersion + "-"
}

// Parse reports whether s is a secret with this wire prefix: the prefix and a
// 43-character base64url secret.
func Parse(prefix, s string) bool {
	rest, found := strings.CutPrefix(s, prefix)
	return found && validSecret(rest)
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
