package memberkeys

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

	"github.com/plat5dev/plat5/identity/metrics"
)

var ErrNotFound = errors.New("member api key not found")

const (
	defaultTimeout  = 5 * time.Second
	storeTracerName = "identity.memberkeys.store"
)

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

// Validated is a member key plus org principal fields for gateway admission.
type Validated struct {
	Key              *APIKey
	OrganizationID   string
	UserID           *string
	ServiceAccountID *string
	MemberStatus     string
}

func (s *Store) Create(ctx context.Context, key *APIKey) error {
	ctx, span := s.tracer.Start(ctx, "db.create_member_api_key")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "create_member_api_key"),
		attribute.String("key.id", key.ID),
	)

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO member_api_keys (id, member_id, name, key_prefix, key_hash, created_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, key.ID, key.MemberID, key.Name, key.KeyPrefix, key.KeyHash, key.CreatedAt, key.RevokedAt)

	metrics.RecordDBOperation("create_member_api_key", time.Since(start), err)
	if err != nil {
		span.RecordError(err)
		if isUniqueViolation(err) {
			span.SetStatus(codes.Error, "hash collision")
			return fmt.Errorf("key hash collision detected")
		}
		span.SetStatus(codes.Error, "insert failed")
		return fmt.Errorf("failed to create member key: %w", err)
	}

	span.SetStatus(codes.Ok, "created")
	return nil
}

func (s *Store) GetByHash(ctx context.Context, keyHash string) (*Validated, error) {
	ctx, span := s.tracer.Start(ctx, "db.get_member_api_key_by_hash")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "get_member_api_key_by_hash"),
	)

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	var key APIKey
	var orgID, status string
	var userID, saID *string
	err := s.pool.QueryRow(ctx, `
		SELECT
			k.id, k.member_id, k.name, k.key_prefix, k.key_hash, k.created_at, k.revoked_at,
			m.organization_id, m.user_id, m.service_account_id, m.status
		FROM member_api_keys k
		INNER JOIN members m ON m.id = k.member_id
		WHERE k.key_hash = $1
	`, keyHash).Scan(
		&key.ID,
		&key.MemberID,
		&key.Name,
		&key.KeyPrefix,
		&key.KeyHash,
		&key.CreatedAt,
		&key.RevokedAt,
		&orgID,
		&userID,
		&saID,
		&status,
	)

	metrics.RecordDBOperation("get_member_api_key_by_hash", time.Since(start), errIfNotNotFound(err))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "query failed")
		return nil, fmt.Errorf("failed to get member key: %w", err)
	}

	span.SetStatus(codes.Ok, "found")
	return &Validated{
		Key:              &key,
		OrganizationID:   orgID,
		UserID:           userID,
		ServiceAccountID: saID,
		MemberStatus:     status,
	}, nil
}

func (s *Store) GetByID(ctx context.Context, memberID, keyID string) (*APIKey, error) {
	ctx, span := s.tracer.Start(ctx, "db.get_member_api_key")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "get_member_api_key"),
		attribute.String("key.id", keyID),
	)

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	var key APIKey
	err := s.pool.QueryRow(ctx, `
		SELECT id, member_id, name, key_prefix, key_hash, created_at, revoked_at
		FROM member_api_keys
		WHERE id = $1 AND member_id = $2
	`, keyID, memberID).Scan(
		&key.ID,
		&key.MemberID,
		&key.Name,
		&key.KeyPrefix,
		&key.KeyHash,
		&key.CreatedAt,
		&key.RevokedAt,
	)

	metrics.RecordDBOperation("get_member_api_key", time.Since(start), errIfNotNotFound(err))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "query failed")
		return nil, fmt.Errorf("failed to get member key: %w", err)
	}

	span.SetStatus(codes.Ok, "found")
	return &key, nil
}

func (s *Store) List(ctx context.Context, memberID string, limit, offset int) ([]*APIKey, bool, error) {
	ctx, span := s.tracer.Start(ctx, "db.list_member_api_keys")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "list_member_api_keys"),
		attribute.String("member.id", memberID),
	)

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	rows, err := s.pool.Query(ctx, `
		SELECT id, member_id, name, key_prefix, created_at, revoked_at
		FROM member_api_keys
		WHERE member_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, memberID, limit+1, offset)
	if err != nil {
		metrics.RecordDBOperation("list_member_api_keys", time.Since(start), err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "query failed")
		return nil, false, fmt.Errorf("failed to list member keys: %w", err)
	}
	defer rows.Close()

	var out []*APIKey
	for rows.Next() {
		var key APIKey
		if err := rows.Scan(
			&key.ID,
			&key.MemberID,
			&key.Name,
			&key.KeyPrefix,
			&key.CreatedAt,
			&key.RevokedAt,
		); err != nil {
			metrics.RecordDBOperation("list_member_api_keys", time.Since(start), err)
			span.RecordError(err)
			span.SetStatus(codes.Error, "scan failed")
			return nil, false, fmt.Errorf("failed to scan member key: %w", err)
		}
		out = append(out, &key)
	}
	if err := rows.Err(); err != nil {
		metrics.RecordDBOperation("list_member_api_keys", time.Since(start), err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "rows error")
		return nil, false, fmt.Errorf("failed to list member keys: %w", err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}

	metrics.RecordDBOperation("list_member_api_keys", time.Since(start), nil)
	span.SetAttributes(attribute.Int("keys.count", len(out)))
	span.SetStatus(codes.Ok, "listed")
	return out, hasMore, nil
}

func (s *Store) Revoke(ctx context.Context, memberID, keyID string) (*APIKey, error) {
	ctx, span := s.tracer.Start(ctx, "db.revoke_member_api_key")
	defer span.End()
	span.SetAttributes(
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "revoke_member_api_key"),
		attribute.String("key.id", keyID),
	)

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	key, err := s.GetByID(ctx, memberID, keyID)
	if err != nil {
		metrics.RecordDBOperation("revoke_member_api_key", time.Since(start), errIfNotNotFound(err))
		if errors.Is(err, ErrNotFound) {
			span.SetStatus(codes.Ok, "not found")
			return nil, ErrNotFound
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "load failed")
		return nil, err
	}
	if key.IsRevoked() {
		metrics.RecordDBOperation("revoke_member_api_key", time.Since(start), nil)
		span.SetStatus(codes.Ok, "already revoked")
		return key, nil
	}

	key.Revoke()
	tag, err := s.pool.Exec(ctx, `
		UPDATE member_api_keys
		SET revoked_at = $1
		WHERE id = $2 AND member_id = $3 AND revoked_at IS NULL
	`, key.RevokedAt, key.ID, memberID)

	metrics.RecordDBOperation("revoke_member_api_key", time.Since(start), err)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "update failed")
		return nil, fmt.Errorf("failed to revoke member key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.GetByID(ctx, memberID, keyID)
	}

	span.SetStatus(codes.Ok, "revoked")
	return key, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func errIfNotNotFound(err error) error {
	if errors.Is(err, ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
