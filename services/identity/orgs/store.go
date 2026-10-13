package orgs

import (
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

var (
	ErrNotFound = errors.New("not found")
	// ErrUserNotFound is a person named in the path who is not in the directory.
	ErrUserNotFound = errors.New("user not found")
	ErrConflict     = errors.New("conflict")
)

type Store struct {
	pool   *pgxpool.Pool
	tracer trace.Tracer
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool:   pool,
		tracer: otel.Tracer("identity.store"),
	}
}
