package id

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Type prefixes (docs/concepts/model.md#ids-and-slugs). Principal ids have
// none; they never leave identity.
const (
	Node    = "n_"
	Member  = "m_"
	Binding = "b_"
	Key     = "k_"
	Invite  = "i_"
)

// New returns prefix + a ULID from crypto/rand.
func New(prefix string) string {
	return prefix + ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

// Valid reports whether s is prefix + a ULID. No case folding.
func Valid(prefix, s string) bool {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return false
	}
	_, err := ulid.ParseStrict(rest)
	return err == nil && strings.ToUpper(rest) == rest
}
