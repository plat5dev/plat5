// Package credentials is every API key and session (docs/identity.md#credential-format).
// A credential belongs to one membership: a user key to the person's directory
// membership, a member key or session to a member. One table, one format, one
// validate. A credential carries its membership's permissions and has none of
// its own.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
)

var (
	ErrNotFound = errors.New("credential not found")
	// ErrNoMembership is a create for a membership that does not exist.
	ErrNoMembership = errors.New("membership not found")
)

// SessionTTL is fixed. Not boot config.
const SessionTTL = time.Hour

const storeTracerName = "identity.credentials.store"

type Credential struct {
	ID           string
	MembershipID string
	Kind         apikey.Kind
	// Name is a key's. Sessions have none.
	Name      *string
	KeyPrefix string
	KeyHash   string
	// ExpiresAt is a session's. Keys do not expire.
	ExpiresAt *time.Time
	// RevokedAt is a key's. Sessions are not revoked; they expire.
	RevokedAt *time.Time
	CreatedAt time.Time
}

// NewKey is a key for membershipID, from its plaintext.
func NewKey(membershipID, name, plaintext, prefix string) *Credential {
	return &Credential{
		ID:           id.New(),
		MembershipID: membershipID,
		Kind:         apikey.KindKey,
		Name:         &name,
		KeyPrefix:    apikey.DisplayPrefix(plaintext, prefix),
		KeyHash:      apikey.Hash(plaintext),
		CreatedAt:    time.Now().UTC(),
	}
}

// NewSession is a session for membershipID, from its plaintext.
func NewSession(membershipID, plaintext, prefix string, now time.Time) *Credential {
	now = now.UTC()
	expires := now.Add(SessionTTL)
	return &Credential{
		ID:           id.New(),
		MembershipID: membershipID,
		Kind:         apikey.KindSession,
		KeyPrefix:    apikey.DisplayPrefix(plaintext, prefix),
		KeyHash:      apikey.Hash(plaintext),
		ExpiresAt:    &expires,
		CreatedAt:    now,
	}
}

// Usable is false once a key is revoked or a session has expired. now ==
// expires_at is expired.
func (c *Credential) Usable(now time.Time) bool {
	if c.RevokedAt != nil {
		return false
	}
	return c.ExpiresAt == nil || now.UTC().Before(c.ExpiresAt.UTC())
}

// Found is a credential with the standing it carries.
type Found struct {
	Credential *Credential
	// OrganizationID is nil for the directory.
	OrganizationID *string
	// IdentityID is the person or service account.
	IdentityID string
	// MemberStatus and MemberRole are an org membership's. Nil in the directory.
	MemberStatus *string
	MemberRole   *string
	// PersonStatus holds in every realm. A service account's is always active.
	PersonStatus string
}

type Store struct {
	pool   *pgxpool.Pool
	tracer trace.Tracer
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, tracer: otel.Tracer(storeTracerName)}
}

func (s *Store) Create(ctx context.Context, c *Credential) error {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_credential", dbx.DefaultTimeout,
		attribute.String("credential.id", c.ID),
		attribute.String("credential.kind", string(c.Kind)),
	)
	defer cancel()
	defer op.End()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO credentials
			(id, membership_id, kind, name, key_prefix, key_hash, expires_at, revoked_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, c.ID, c.MembershipID, c.Kind, c.Name, c.KeyPrefix, c.KeyHash, c.ExpiresAt, c.RevokedAt, c.CreatedAt)
	if err != nil {
		if dbx.IsUniqueViolation(err) {
			return op.Fail(fmt.Errorf("credential hash collision"))
		}
		if dbx.IsForeignKeyViolation(err) {
			return op.Expected("no membership", ErrNoMembership)
		}
		return op.Fail(err)
	}
	op.OK("created")
	return nil
}

// Lookup finds a credential by hash, with its membership and person. A
// credential of a person whose account is gone is not found.
func (s *Store) Lookup(ctx context.Context, hash string) (*Found, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "lookup_credential", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	var c Credential
	var f Found
	err := s.pool.QueryRow(ctx, `
		SELECT
			c.id, c.membership_id, c.kind, c.name, c.key_prefix, c.key_hash,
			c.expires_at, c.revoked_at, c.created_at,
			m.organization_id, m.identity_id, m.status, m.role, i.status
		FROM credentials c
		INNER JOIN memberships m ON m.id = c.membership_id
		INNER JOIN identities i ON i.id = m.identity_id
		WHERE c.key_hash = $1
	`, hash).Scan(
		&c.ID, &c.MembershipID, &c.Kind, &c.Name, &c.KeyPrefix, &c.KeyHash,
		&c.ExpiresAt, &c.RevokedAt, &c.CreatedAt,
		&f.OrganizationID, &f.IdentityID, &f.MemberStatus, &f.MemberRole, &f.PersonStatus,
	)
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	f.Credential = &c
	op.OK("found")
	return &f, nil
}

// ListKeys lists a membership's keys, revoked ones included. Never sessions.
func (s *Store) ListKeys(ctx context.Context, membershipID string, limit int, startingAfter string) ([]*Credential, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_keys", dbx.DefaultTimeout,
		attribute.String("membership.id", membershipID),
	)
	defer cancel()
	defer op.End()

	var after any
	if startingAfter != "" {
		after = startingAfter
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, membership_id, kind, name, key_prefix, created_at, revoked_at
		FROM credentials
		WHERE membership_id = $1 AND kind = 'key' AND ($2::text IS NULL OR id > $2)
		ORDER BY id ASC
		LIMIT $3
	`, membershipID, after, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer rows.Close()

	var out []*Credential
	for rows.Next() {
		var c Credential
		if err := rows.Scan(&c.ID, &c.MembershipID, &c.Kind, &c.Name, &c.KeyPrefix, &c.CreatedAt, &c.RevokedAt); err != nil {
			return nil, false, op.Fail(err)
		}
		out = append(out, &c)
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

// RevokeKey soft-revokes idempotently (COALESCE keeps the first revoked_at).
// A key under another membership is not found.
func (s *Store) RevokeKey(ctx context.Context, membershipID, keyID string) (*Credential, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "revoke_key", dbx.DefaultTimeout,
		attribute.String("credential.id", keyID),
	)
	defer cancel()
	defer op.End()

	var c Credential
	err := s.pool.QueryRow(ctx, `
		UPDATE credentials
		SET revoked_at = COALESCE(revoked_at, $1)
		WHERE id = $2 AND membership_id = $3 AND kind = 'key'
		RETURNING id, membership_id, kind, name, key_prefix, created_at, revoked_at
	`, time.Now().UTC(), keyID, membershipID).Scan(
		&c.ID, &c.MembershipID, &c.Kind, &c.Name, &c.KeyPrefix, &c.CreatedAt, &c.RevokedAt,
	)
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("revoked")
	return &c, nil
}
