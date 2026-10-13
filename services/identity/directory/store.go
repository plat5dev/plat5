// Package directory is the realm every person belongs to (docs/model.md). A
// person is an identity with a directory membership of the same id, which
// holds their global display name and their own keys. Private to customers:
// only staff list people.
package directory

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
)

var ErrNotFound = errors.New("user not found")

const (
	StatusActive    = "active"
	StatusSuspended = "suspended"

	MaxDisplayNameLen = 128
	MaxIssuerLen      = 512
	// MaxSubjectLen is OIDC's limit on sub.
	MaxSubjectLen = 255
)

const storeTracerName = "identity.directory.store"

type User struct {
	ID          string
	DisplayName *string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Store struct {
	pool   *pgxpool.Pool
	tracer trace.Tracer
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, tracer: otel.Tracer(storeTracerName)}
}

// Resolve returns the person linked to the IdP issuer and subject, and their
// status. A subject seen for the first time creates the person, with their
// directory membership. A person carried over from before the directory has
// the subject as their id and no link yet; the first resolve links them.
func (s *Store) Resolve(ctx context.Context, issuer, subject string) (userID, status string, err error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "resolve_idp_subject", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	userID, status, err = lookupLink(ctx, s.pool, issuer, subject)
	if err == nil {
		op.OK("linked")
		return userID, status, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", "", op.Fail(err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", op.Fail(err)
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, `
		INSERT INTO idp_links (issuer, subject, identity_id)
		SELECT $1, $2, i.id
		FROM identities i
		WHERE i.id = $2 AND i.kind = 'user'
		AND NOT EXISTS (SELECT 1 FROM idp_links l WHERE l.identity_id = i.id)
		ON CONFLICT DO NOTHING
		RETURNING identity_id
	`, issuer, subject).Scan(&userID)
	if err == nil {
		if err := tx.QueryRow(ctx, `SELECT status FROM identities WHERE id = $1`, userID).Scan(&status); err != nil {
			return "", "", op.Fail(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", "", op.Fail(err)
		}
		op.OK("carried over")
		return userID, status, nil
	}
	if !dbx.IsNoRows(err) {
		return "", "", op.Fail(err)
	}

	userID = id.New()
	if err := insertPerson(ctx, tx, userID); err != nil {
		return "", "", op.Fail(err)
	}
	var linked string
	err = tx.QueryRow(ctx, `
		INSERT INTO idp_links (issuer, subject, identity_id)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING
		RETURNING identity_id
	`, issuer, subject, userID).Scan(&linked)
	if dbx.IsNoRows(err) {
		// A concurrent resolve linked the subject first. Use that person.
		_ = tx.Rollback(ctx)
		userID, status, err = lookupLink(ctx, s.pool, issuer, subject)
		if err != nil {
			return "", "", op.Fail(err)
		}
		op.OK("linked concurrently")
		return userID, status, nil
	}
	if err != nil {
		return "", "", op.Fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", op.Fail(err)
	}
	op.Attr(attribute.String("user.id", userID))
	op.OK("created")
	return userID, StatusActive, nil
}

func lookupLink(ctx context.Context, q dbx.Querier, issuer, subject string) (string, string, error) {
	var userID, status string
	err := q.QueryRow(ctx, `
		SELECT i.id, i.status
		FROM idp_links l
		INNER JOIN identities i ON i.id = l.identity_id
		WHERE l.issuer = $1 AND l.subject = $2
	`, issuer, subject).Scan(&userID, &status)
	if dbx.IsNoRows(err) {
		return "", "", ErrNotFound
	}
	return userID, status, err
}

func insertPerson(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO identities (id, kind) VALUES ($1, 'user')`, userID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO memberships (id, identity_id, organization_id, principal)
		VALUES ($1, $1, NULL, 'user')
	`, userID)
	return err
}

const userSelect = `
	SELECT i.id, d.display_name, i.status, i.created_at, i.updated_at
	FROM identities i
	INNER JOIN memberships d ON d.id = i.id AND d.organization_id IS NULL
`

func (s *Store) Get(ctx context.Context, userID string) (*User, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_user", dbx.DefaultTimeout,
		attribute.String("user.id", userID),
	)
	defer cancel()
	defer op.End()

	u, err := scanUser(s.pool.QueryRow(ctx, userSelect+`WHERE i.id = $1 AND i.kind = 'user'`, userID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, op.Expected("not found", ErrNotFound)
		}
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return u, nil
}

// List is one page of every person, by id. For staff only.
func (s *Store) List(ctx context.Context, limit int, startingAfter string) ([]*User, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_users", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	var after any
	if startingAfter != "" {
		after = startingAfter
	}
	rows, err := s.pool.Query(ctx, userSelect+`
		WHERE i.kind = 'user' AND ($1::text IS NULL OR i.id > $1)
		ORDER BY i.id ASC
		LIMIT $2
	`, after, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer rows.Close()

	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, false, op.Fail(err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, false, op.Fail(err)
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	op.Attr(attribute.Int("users.count", len(out)))
	op.OK("ok")
	return out, hasMore, nil
}

// Update is a patch. Nil leaves a field as it is. ClearName sets the display
// name to null.
type Update struct {
	DisplayName *string
	ClearName   bool
	Status      *string
}

// Update applies u. It also returns the person as they were, read under the lock.
func (s *Store) Update(ctx context.Context, userID string, u Update) (*User, User, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "update_user", dbx.DefaultTimeout,
		attribute.String("user.id", userID),
	)
	defer cancel()
	defer op.End()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, User{}, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	user, err := scanUser(tx.QueryRow(ctx, userSelect+`
		WHERE i.id = $1 AND i.kind = 'user'
		FOR UPDATE OF i, d
	`, userID))
	if err != nil {
		if dbx.IsNoRows(err) {
			return nil, User{}, op.Expected("not found", ErrNotFound)
		}
		return nil, User{}, op.Fail(err)
	}
	prior := *user

	if u.ClearName {
		user.DisplayName = nil
	} else if u.DisplayName != nil {
		user.DisplayName = u.DisplayName
	}
	if u.Status != nil {
		user.Status = *u.Status
	}
	user.UpdatedAt = time.Now().UTC()

	if _, err := tx.Exec(ctx, `
		UPDATE identities SET status = $2, updated_at = $3 WHERE id = $1
	`, userID, user.Status, user.UpdatedAt); err != nil {
		return nil, User{}, op.Fail(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memberships SET display_name = $2, updated_at = $3 WHERE id = $1
	`, userID, user.DisplayName, user.UpdatedAt); err != nil {
		return nil, User{}, op.Fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, User{}, op.Fail(err)
	}
	op.OK("ok")
	return user, prior, nil
}

func scanUser(row dbx.Scannable) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.DisplayName, &u.Status, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}
