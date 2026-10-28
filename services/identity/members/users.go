package members

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/nodes"
)

// User is a person by their user id: the id user routes carry. The principal
// id behind it never leaves identity.
type User struct {
	ID     string
	Name   *string
	Status string
	// principalID is the person's principal.
	principalID string
}

const userSelect = `SELECT user_id, name, status, id FROM principals WHERE kind = 'person' AND `

func scanUser(row dbx.Scannable) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Name, &u.Status, &u.principalID); err != nil {
		return nil, err
	}
	return &u, nil
}

// user reads a person by user id, or ErrNotFound.
func (s *Store) user(ctx context.Context, q dbx.Querier, userID string) (*User, error) {
	if !id.Valid(userID) {
		return nil, ErrNotFound
	}
	u, err := scanUser(q.QueryRow(ctx, userSelect+`user_id = $1`, userID))
	if dbx.IsNoRows(err) {
		return nil, ErrNotFound
	}
	return u, err
}

// GetUser is GET /users/{user_id}.
func (s *Store) GetUser(ctx context.Context, userID string) (*User, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_user", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	u, err := s.user(ctx, s.pool, userID)
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	op.OK("ok")
	return u, nil
}

// LookupUser is the person linked to an IdP subject, or nil.
func (s *Store) LookupUser(ctx context.Context, issuer, subject string) (*User, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "lookup_user", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	u, err := scanUser(s.pool.QueryRow(ctx, `
		SELECT p.user_id, p.name, p.status, p.id
		FROM idp_links l JOIN principals p ON p.id = l.principal_id
		WHERE l.issuer = $1 AND l.subject = $2
	`, issuer, subject))
	if dbx.IsNoRows(err) {
		op.OK("none")
		return nil, nil
	}
	if err != nil {
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return u, nil
}

// MemberUser is the user behind a person's member, removed members included.
// A service account's member, or one whose account was deleted, has none.
func (s *Store) MemberUser(ctx context.Context, memberID string) (*User, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_member_user", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if !id.Valid(memberID) {
		return nil, op.Expected("not found", ErrNotFound)
	}
	u, err := scanUser(s.pool.QueryRow(ctx, `
		SELECT p.user_id, p.name, p.status, p.id
		FROM members m JOIN principals p ON p.id = m.principal_id
		WHERE m.id = $1 AND p.kind = 'person'
	`, memberID))
	if dbx.IsNoRows(err) {
		return nil, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return u, nil
}

// SetStatus suspends or reactivates a person. It returns the status before.
func (s *Store) SetStatus(ctx context.Context, userID, status string) (string, *User, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "set_user_status", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if status != StatusActive && status != StatusSuspended {
		return "", nil, op.Expected("invalid", errors.FieldError("status", errors.FallbackValidation))
	}
	if !id.Valid(userID) {
		return "", nil, op.Expected("not found", ErrNotFound)
	}
	var u User
	var before string
	err := s.pool.QueryRow(ctx, `
		UPDATE principals p SET status = $2, updated_at = CASE WHEN old.status = $2 THEN p.updated_at ELSE $3 END
		FROM (SELECT id, status FROM principals WHERE user_id = $1 AND kind = 'person' FOR UPDATE) old
		WHERE p.id = old.id
		RETURNING p.user_id, p.name, p.status, p.id, old.status
	`, userID, status, s.now().UTC()).Scan(&u.ID, &u.Name, &u.Status, &u.principalID, &before)
	if dbx.IsNoRows(err) {
		return "", nil, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return "", nil, op.Fail(err)
	}
	op.OK("ok")
	return before, &u, nil
}

// DeleteAccount deletes a person (docs/concepts/lifecycle.md#deleting-an-account):
// every member they have is removed, with member.removed per tree, and the
// principal goes with its IdP links and personal keys, with user.deleted.
// Removed member rows stay, with no name. An account already deleted is left
// as it is: changed is false.
func (s *Store) DeleteAccount(ctx context.Context, userID string) (bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_account", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return false, op.Fail(err)
	}
	defer w.rollback(ctx)
	u, err := s.user(ctx, w.tx, userID)
	if err == ErrNotFound {
		op.OK("unchanged")
		return false, nil
	}
	if err != nil {
		return false, op.Fail(err)
	}
	rows, err := w.tx.Query(ctx, `SELECT DISTINCT root_id FROM members WHERE principal_id = $1 AND removed_at IS NULL`, u.principalID)
	if err != nil {
		return false, op.Fail(err)
	}
	roots, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, op.Fail(err)
	}
	if err := nodes.LockTrees(ctx, w.tx, roots); err != nil {
		return false, op.Fail(err)
	}
	rows, err = w.tx.Query(ctx, `
		SELECT id FROM members WHERE principal_id = $1 AND removed_at IS NULL ORDER BY id
	`, u.principalID)
	if err != nil {
		return false, op.Fail(err)
	}
	memberIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, op.Fail(err)
	}
	for _, mid := range memberIDs {
		row, err := s.lockedMember(ctx, w, mid)
		if err != nil {
			return false, op.Fail(err)
		}
		if err := s.removeMember(ctx, w, row); err != nil {
			return false, op.Fail(err)
		}
	}
	tag, err := w.tx.Exec(ctx, `DELETE FROM principals WHERE id = $1`, u.principalID)
	if err != nil {
		return false, op.Fail(err)
	}
	if tag.RowsAffected() == 0 {
		// A concurrent delete got there first and wrote the events.
		op.OK("unchanged")
		return false, nil
	}
	w.userEvent(lifecycle.UserDeleted(u.ID))
	if err := w.commit(ctx); err != nil {
		return false, op.Fail(err)
	}
	op.OK("deleted")
	return true, nil
}
