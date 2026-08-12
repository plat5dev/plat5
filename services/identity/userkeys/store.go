package userkeys

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/internal/dbx"
)

var ErrNotFound = errors.New("user api key not found")

const storeTracerName = "identity.userkeys.store"

type Store struct {
	pool   *pgxpool.Pool
	tracer trace.Tracer
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool:   pool,
		tracer: otel.Tracer(storeTracerName),
	}
}

func (s *Store) Create(ctx context.Context, key *APIKey) error {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_user_api_key", dbx.DefaultTimeout,
		attribute.String("key.id", key.ID),
	)
	defer cancel()
	defer op.End()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_api_keys (id, user_id, name, key_prefix, key_hash, created_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, key.ID, key.UserID, key.Name, key.KeyPrefix, key.KeyHash, key.CreatedAt, key.RevokedAt)
	if err != nil {
		if dbx.IsUniqueViolation(err) {
			return op.Fail(fmt.Errorf("key hash collision detected"))
		}
		return op.Fail(err)
	}
	op.OK("created")
	return nil
}

func (s *Store) GetByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_user_api_key_by_hash", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	key, err := scanKey(s.pool.QueryRow(ctx, `
		SELECT id, user_id, name, key_prefix, key_hash, created_at, revoked_at
		FROM user_api_keys
		WHERE key_hash = $1
	`, keyHash))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("found")
	return key, nil
}

func (s *Store) GetByID(ctx context.Context, userID, keyID string) (*APIKey, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_user_api_key", dbx.DefaultTimeout,
		attribute.String("key.id", keyID),
	)
	defer cancel()
	defer op.End()

	key, err := scanKey(s.pool.QueryRow(ctx, `
		SELECT id, user_id, name, key_prefix, key_hash, created_at, revoked_at
		FROM user_api_keys
		WHERE id = $1 AND user_id = $2
	`, keyID, userID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("found")
	return key, nil
}

func (s *Store) List(ctx context.Context, userID string, limit, offset int) ([]*APIKey, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_user_api_keys", dbx.DefaultTimeout,
		attribute.String("user.id", userID),
	)
	defer cancel()
	defer op.End()

	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, name, key_prefix, created_at, revoked_at
		FROM user_api_keys
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, userID, limit+1, offset)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer rows.Close()

	var out []*APIKey
	for rows.Next() {
		key, err := scanKeyList(rows)
		if err != nil {
			return nil, false, op.Fail(err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, false, op.Fail(err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	op.Attr(attribute.Int("keys.count", len(out)))
	op.OK("listed")
	return out, hasMore, nil
}

// Revoke soft-revokes idempotently (COALESCE keeps first revoked_at).
func (s *Store) Revoke(ctx context.Context, userID, keyID string) (*APIKey, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "revoke_user_api_key", dbx.DefaultTimeout,
		attribute.String("key.id", keyID),
	)
	defer cancel()
	defer op.End()

	now := time.Now().UTC()
	key, err := scanKey(s.pool.QueryRow(ctx, `
		UPDATE user_api_keys
		SET revoked_at = COALESCE(revoked_at, $1)
		WHERE id = $2 AND user_id = $3
		RETURNING id, user_id, name, key_prefix, key_hash, created_at, revoked_at
	`, now, keyID, userID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("revoked")
	return key, nil
}

func scanKey(row dbx.Scannable) (*APIKey, error) {
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
		if dbx.IsNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &key, nil
}

func scanKeyList(row dbx.Scannable) (*APIKey, error) {
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
		if dbx.IsNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &key, nil
}
