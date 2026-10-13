package apikey

import "strings"

// Kind is what a credential is: a key is revoked, a session expires. The
// prefix says which. It does not say whose (docs/identity.md#credential-format).
type Kind string

const (
	KindKey     Kind = "key"
	KindSession Kind = "session"
)

const (
	version   = "2"
	secretLen = 43 // RandomBytes, base64url without padding
)

// v1 kinds still validate. sk was a user key, mk a member key, ms a session.
var v1Kinds = map[string]Kind{"sk": KindKey, "mk": KindKey, "ms": KindSession}

// Prefix is the wire prefix identity mints for kind: {brand}-k-2- or {brand}-s-2-.
func Prefix(brand string, kind Kind) string {
	letter := "k"
	if kind == KindSession {
		letter = "s"
	}
	return brand + "-" + letter + "-" + version + "-"
}

// Parse reads s as a credential of this brand: its kind and wire prefix. ok is
// false for any other shape or brand, which validate answers as not valid.
func Parse(brand, s string) (kind Kind, prefix string, ok bool) {
	rest, found := strings.CutPrefix(s, brand+"-")
	if !found {
		return "", "", false
	}
	letters, rest, found := strings.Cut(rest, "-")
	if !found {
		return "", "", false
	}
	ver, secret, found := strings.Cut(rest, "-")
	if !found || !validSecret(secret) {
		return "", "", false
	}
	switch {
	case ver == version && letters == "k":
		kind = KindKey
	case ver == version && letters == "s":
		kind = KindSession
	case ver == "1" && v1Kinds[letters] != "":
		kind = v1Kinds[letters]
	default:
		return "", "", false
	}
	return kind, s[:len(s)-len(secret)], true
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
