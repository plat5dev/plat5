package dbx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Unavailable is Postgres unreachable or not answering, a 503; any other
// failure is a 500.
func TestUnavailable(t *testing.T) {
	for err, want := range map[error]bool{
		fmt.Errorf("q: %w", context.DeadlineExceeded): true,
		io.ErrUnexpectedEOF:                           true,
		&pgconn.PgError{Code: "08006"}:                true,
		&pgconn.PgError{Code: "57P01"}:                true,
		&pgconn.PgError{Code: "53300"}:                true,
		&pgconn.PgError{Code: "23505"}:                false,
		&pgconn.PgError{Code: "42703"}:                false,
		errors.New("scan: wrong type"):                false,
		context.Canceled:                              false,
	} {
		if got := Unavailable(err); got != want {
			t.Errorf("Unavailable(%v) = %v, want %v", err, got, want)
		}
	}
	if Unavailable(nil) {
		t.Error("Unavailable(nil)")
	}
}
