package id

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

// New returns a ULID string using crypto/rand.
func New() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

// Valid reports whether s is a ULID. No case folding.
func Valid(s string) bool {
	_, err := ulid.Parse(s)
	return err == nil
}
