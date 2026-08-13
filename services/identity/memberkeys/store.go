package memberkeys

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

var ErrNotFound = errors.New("member api key not found")

const storeTracerName = "identity.memberkeys.store"

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

// Validated is a member key plus org fields for gateway admission.
type Validated struct {
	Key            *APIKey
	OrganizationID string
	MemberStatus   string
}

func (s *Store) Create(ctx context.Context, key *APIKey) error {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_member_api_key", dbx.DefaultTimeout,
		attribute.String("key.id", key.ID),
	)
	defer cancel()
	defer op.End()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO member_api_keys (id, member_id, name, key_prefix, key_hash, created_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, key.ID, key.MemberID, key.Name, key.KeyPrefix, key.KeyHash, key.CreatedAt, key.RevokedAt)
	if err != nil {
		if dbx.IsUniqueViolation(err) {
			return op.Fail(fmt.Errorf("key hash collision detected"))
		}
		return op.Fail(err)
	}
	op.OK("created")
	return nil
}

func (s *Store) GetByHash(ctx context.Context, keyHash string) (*Validated, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_member_api_key_by_hash", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	var key APIKey
	var orgID, status string
	err := s.pool.QueryRow(ctx, `
		SELECT
			k.id, k.member_id, k.name, k.key_prefix, k.key_hash, k.created_at, k.revoked_at,
			m.organization_id, m.status
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
		&status,
	)
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("found")
	return &Validated{
		Key:            &key,
		OrganizationID: orgID,
		MemberStatus:   status,
	}, nil
}

func (s *Store) List(ctx context.Context, memberID string, limit, offset int) ([]*APIKey, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_member_api_keys", dbx.DefaultTimeout,
		attribute.String("member.id", memberID),
	)
	defer cancel()
	defer op.End()

	rows, err := s.pool.Query(ctx, `
		SELECT id, member_id, name, key_prefix, created_at, revoked_at
		FROM member_api_keys
		WHERE member_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, memberID, limit+1, offset)
	if err != nil {
		return nil, false, op.Fail(err)
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
			return nil, false, op.Fail(err)
		}
		out = append(out, &key)
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
func (s *Store) Revoke(ctx context.Context, memberID, keyID string) (*APIKey, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "revoke_member_api_key", dbx.DefaultTimeout,
		attribute.String("key.id", keyID),
	)
	defer cancel()
	defer op.End()

	now := time.Now().UTC()
	key, err := scanKey(s.pool.QueryRow(ctx, `
		UPDATE member_api_keys
		SET revoked_at = COALESCE(revoked_at, $1)
		WHERE id = $2 AND member_id = $3
		RETURNING id, member_id, name, key_prefix, key_hash, created_at, revoked_at
	`, now, keyID, memberID))
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
		&key.MemberID,
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
