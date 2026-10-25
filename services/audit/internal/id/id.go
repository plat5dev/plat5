package id

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// NewAt returns a lowercase ULID whose time part is t, so id order is t order.
func NewAt(t time.Time) string {
	return strings.ToLower(ulid.MustNew(ulid.Timestamp(t), rand.Reader).String())
}

// Valid reports whether s is a lowercase ULID. An uppercase one isn't
// canonical and doesn't match.
func Valid(s string) bool {
	_, err := ulid.ParseStrict(s)
	return err == nil && s == strings.ToLower(s)
}

// Time returns the time part of a valid ULID.
func Time(s string) time.Time {
	return ulid.Time(ulid.MustParse(s).Time())
}
