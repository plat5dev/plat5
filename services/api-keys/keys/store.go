package keys

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/api-keys/metrics"
)

var ErrNotFound = errors.New("key not found")

const (
	defaultTimeout  = 5 * time.Second
	storeTracerName = "api-keys.store"
)

type KeyStore interface {
	Create(ctx context.Context, key *APIKey) error
	GetByHash(ctx context.Context, keyHash string) (*APIKey, error)
	GetByID(ctx context.Context, userID, keyID string) (*APIKey, error)
	ListByUser(ctx context.Context, userID string, limit, offset int) ([]*APIKey, bool, error)
	Revoke(ctx context.Context, key *APIKey) error
}

type Store struct {
	pool   *pgxpool.Pool
	tracer trace.Tracer
}

var _ KeyStore = (*Store)(nil)

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool:   pool,
		tracer: otel.Tracer(storeTracerName),
	}
}

func (s *Store) Create(ctx context.Context, key *APIKey) error {
	ctx, span := s.tracer.Start(ctx, "db.create")
	defer span.End()

	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "create"),
		attribute.String("key.id", key.ID),
	)

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO api_keys (id, user_id, name, key_prefix, key_hash, created_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, key.ID, key.UserID, key.Name, key.KeyPrefix, key.KeyHash, key.CreatedAt, key.RevokedAt)

	metrics.RecordDBOperation("create", time.Since(start), err)

	if err != nil {
		span.RecordError(err)
		if isUniqueViolation(err) {
			span.SetStatus(codes.Error, "hash collision")
			return fmt.Errorf("key hash collision detected")
		}
		span.SetStatus(codes.Error, "insert failed")
		return fmt.Errorf("failed to create key: %w", err)
	}

	span.SetStatus(codes.Ok, "created")
	return nil
}

func (s *Store) GetByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	ctx, span := s.tracer.Start(ctx, "db.get_by_hash")
	defer span.End()

	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "get"),
		attribute.String("key.hash_prefix", keyHash[:8]),
	)

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	key, err := scanKey(s.pool.QueryRow(ctx, `
		SELECT id, user_id, name, key_prefix, key_hash, created_at, revoked_at
		FROM api_keys
		WHERE key_hash = $1
	`, keyHash))

	metrics.RecordDBOperation("get", time.Since(start), errIfNotNotFound(err))

	if err != nil {
		if errors.Is(err, ErrNotFound) {
			span.SetAttributes(attribute.Bool("key.found", false))
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "query failed")
		return nil, fmt.Errorf("failed to get key: %w", err)
	}

	span.SetAttributes(attribute.Bool("key.found", true))
	span.SetStatus(codes.Ok, "found")
	return key, nil
}

func (s *Store) GetByID(ctx context.Context, userID, keyID string) (*APIKey, error) {
	ctx, span := s.tracer.Start(ctx, "db.get_by_id")
	defer span.End()

	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "get"),
		attribute.String("key.id", keyID),
	)

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	key, err := scanKey(s.pool.QueryRow(ctx, `
		SELECT id, user_id, name, key_prefix, key_hash, created_at, revoked_at
		FROM api_keys
		WHERE id = $1 AND user_id = $2
	`, keyID, userID))

	metrics.RecordDBOperation("get", time.Since(start), errIfNotNotFound(err))

	if err != nil {
		if errors.Is(err, ErrNotFound) {
			span.SetAttributes(attribute.Bool("key.found", false))
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "query failed")
		return nil, fmt.Errorf("failed to get key: %w", err)
	}

	span.SetAttributes(attribute.Bool("key.found", true))
	span.SetStatus(codes.Ok, "found")
	return key, nil
}

func (s *Store) ListByUser(ctx context.Context, userID string, limit, offset int) ([]*APIKey, bool, error) {
	ctx, span := s.tracer.Start(ctx, "db.list_by_user")
	defer span.End()

	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "scan"),
		attribute.String("user.id", userID),
	)

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, name, key_prefix, created_at, revoked_at
		FROM api_keys
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, userID, limit+1, offset)
	if err != nil {
		metrics.RecordDBOperation("scan", time.Since(start), err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "query failed")
		return nil, false, fmt.Errorf("failed to list keys: %w", err)
	}
	defer rows.Close()

	var out []*APIKey
	for rows.Next() {
		key, err := scanKeyList(rows)
		if err != nil {
			metrics.RecordDBOperation("scan", time.Since(start), err)
			span.RecordError(err)
			span.SetStatus(codes.Error, "scan failed")
			return nil, false, fmt.Errorf("failed to scan key: %w", err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		metrics.RecordDBOperation("scan", time.Since(start), err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "rows error")
		return nil, false, fmt.Errorf("failed to list keys: %w", err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}

	metrics.RecordDBOperation("scan", time.Since(start), nil)
	span.SetAttributes(attribute.Int("keys.count", len(out)))
	span.SetStatus(codes.Ok, "listed")
	return out, hasMore, nil
}

func (s *Store) Revoke(ctx context.Context, key *APIKey) error {
	ctx, span := s.tracer.Start(ctx, "db.revoke")
	defer span.End()

	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "update"),
		attribute.String("key.id", key.ID),
	)

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	key.Revoke()

	tag, err := s.pool.Exec(ctx, `
		UPDATE api_keys
		SET revoked_at = $1
		WHERE id = $2 AND user_id = $3 AND revoked_at IS NULL
	`, key.RevokedAt, key.ID, key.UserID)

	metrics.RecordDBOperation("update", time.Since(start), err)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "update failed")
		return fmt.Errorf("failed to revoke key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		existing, getErr := s.GetByID(ctx, key.UserID, key.ID)
		if getErr != nil {
			span.RecordError(getErr)
			span.SetStatus(codes.Error, "revoke miss")
			return getErr
		}
		key.RevokedAt = existing.RevokedAt
		span.SetStatus(codes.Ok, "already revoked")
		return nil
	}

	span.SetStatus(codes.Ok, "revoked")
	return nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanKey(row scannable) (*APIKey, error) {
	var key APIKey
	err := row.Scan(
		&key.ID,
		&key.UserID,
		&key.Name,
		&key.KeyPrefix,
		&key.KeyHash,
		&key.CreatedAt,
		&key.RevokedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &key, nil
}

// scanKeyList omits key_hash — list responses never need it.
func scanKeyList(row scannable) (*APIKey, error) {
	var key APIKey
	err := row.Scan(
		&key.ID,
		&key.UserID,
		&key.Name,
		&key.KeyPrefix,
		&key.CreatedAt,
		&key.RevokedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &key, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func errIfNotNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
