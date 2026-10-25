package id

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// New returns a lowercase ULID from crypto/rand. Every id has this one form,
// with no type prefix (docs/concepts/model.md#ids-and-slugs).
func New() string {
	return strings.ToLower(ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String())
}

// Valid reports whether s is a lowercase ULID. An uppercase one isn't
// canonical and matches nothing.
func Valid(s string) bool {
	_, err := ulid.ParseStrict(s)
	return err == nil && strings.ToLower(s) == s
}

// IsID reports whether a path ref is read as an id rather than a slug: ids
// start with a digit, slugs with a letter.
func IsID(ref string) bool {
	return ref != "" && ref[0] >= '0' && ref[0] <= '9'
}
