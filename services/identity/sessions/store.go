package sessions

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/dbx"
)

var ErrNotFound = errors.New("member session not found")

const storeTracerName = "identity.sessions.store"

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

// Validated is a session plus the member's org and status.
// No user_id. The gateway must not learn one from this lookup.
type Validated struct {
	Session        *Session
	OrganizationID string
	MemberStatus   string
}

func (s *Store) Create(ctx context.Context, session *Session) error {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_member_session", dbx.DefaultTimeout,
		attribute.String("session.id", session.ID),
		attribute.String("member.id", session.MemberID),
	)
	defer cancel()
	defer op.End()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO member_sessions (id, member_id, token_prefix, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, session.ID, session.MemberID, session.TokenPrefix, session.TokenHash, session.ExpiresAt, session.CreatedAt)
	if err != nil {
		if dbx.IsUniqueViolation(err) {
			return op.Fail(fmt.Errorf("session token hash collision"))
		}
		return op.Fail(err)
	}
	op.OK("created")
	return nil
}

func (s *Store) GetByHash(ctx context.Context, tokenHash string) (*Validated, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_member_session_by_hash", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	var session Session
	var orgID, status string
	err := s.pool.QueryRow(ctx, `
		SELECT
			s.id, s.member_id, s.token_prefix, s.token_hash, s.expires_at, s.created_at,
			m.organization_id, m.status
		FROM member_sessions s
		INNER JOIN members m ON m.id = s.member_id
		WHERE s.token_hash = $1
	`, tokenHash).Scan(
		&session.ID,
		&session.MemberID,
		&session.TokenPrefix,
		&session.TokenHash,
		&session.ExpiresAt,
		&session.CreatedAt,
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
		Session:        &session,
		OrganizationID: orgID,
		MemberStatus:   status,
	}, nil
}

func HashToken(token string) string {
	return apikey.Hash(token)
}
