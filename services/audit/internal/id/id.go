package id

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

// New returns a ULID string using crypto/rand.
func New() string {
	return NewAt(time.Now())
}

// NewAt returns a ULID whose time part is t, so id order is t order.
func NewAt(t time.Time) string {
	return ulid.MustNew(ulid.Timestamp(t), rand.Reader).String()
}

// Valid reports whether s is a ULID. No case folding.
func Valid(s string) bool {
	_, err := ulid.Parse(s)
	return err == nil
}

// Time returns the time part of a valid ULID.
func Time(s string) time.Time {
	return ulid.Time(ulid.MustParse(s).Time())
}
