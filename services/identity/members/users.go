package members

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
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

// SetName sets a person's name, or clears it when name is nil
// (docs/reference/idp.md#a-persons-name). Under users.name.sync
// each_sign_in the IdP owns names, so any edit is refused, whoever sends it.
// It returns the name before.
func (s *Store) SetName(ctx context.Context, snap *feed.Snapshot, userID string, name *string) (*string, *User, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "set_user_name", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if !id.Valid(userID) {
		return nil, nil, op.Expected("not found", ErrNotFound)
	}
	w, err := s.begin(ctx)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	u, err := scanUser(w.tx.QueryRow(ctx, userSelect+`user_id = $1 FOR UPDATE`, userID))
	switch {
	case dbx.IsNoRows(err):
		return nil, nil, op.Expected("not found", ErrNotFound)
	case err != nil:
		return nil, nil, op.Fail(err)
	case snap.NameSync == feed.EachSignIn:
		return nil, nil, op.Expected("idp owns names", errors.NameOwnedByIdPError())
	}
	if name != nil {
		n, err := normalizeName(*name)
		if err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
		name = &n
	}
	before := u.Name
	if _, err := w.tx.Exec(ctx, `
		UPDATE principals SET name = $2, updated_at = CASE WHEN name IS NOT DISTINCT FROM $2 THEN updated_at ELSE $3 END
		WHERE id = $1
	`, u.principalID, name, s.now().UTC()); err != nil {
		return nil, nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	u.Name = name
	op.OK("ok")
	return before, u, nil
}

// removeMembers removes each active member of memberIDs, whose trees the
// caller has locked.
func (s *Store) removeMembers(ctx context.Context, w *write, memberIDs []string) error {
	for _, mid := range memberIDs {
		row, err := s.lockedMember(ctx, w, mid)
		if err != nil {
			return err
		}
		if row.removed {
			continue
		}
		if err := s.removeMember(ctx, w, row); err != nil {
			return err
		}
	}
	return nil
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
	if err := s.removeMembers(ctx, w, memberIDs); err != nil {
		return false, op.Fail(err)
	}
	// A join into a tree not locked above can still insert a member. Locking
	// the principal waits for any join that already inserted one (its foreign
	// key holds the row), and makes any later join wait and then fail the
	// foreign key once this commits. Members that appeared meanwhile are
	// removed too, with their trees locked.
	if _, err := w.tx.Exec(ctx, `SELECT 1 FROM principals WHERE id = $1 FOR UPDATE`, u.principalID); err != nil {
		return false, op.Fail(err)
	}
	rows, err = w.tx.Query(ctx, `
		SELECT root_id, id FROM members WHERE principal_id = $1 AND removed_at IS NULL ORDER BY id
	`, u.principalID)
	if err != nil {
		return false, op.Fail(err)
	}
	late, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var out [2]string
		err := r.Scan(&out[0], &out[1])
		return out, err
	})
	if err != nil {
		return false, op.Fail(err)
	}
	if len(late) > 0 {
		lateRoots := make([]string, len(late))
		lateIDs := make([]string, len(late))
		for i, m := range late {
			lateRoots[i], lateIDs[i] = m[0], m[1]
		}
		if err := nodes.LockTrees(ctx, w.tx, lateRoots); err != nil {
			return false, op.Fail(err)
		}
		if err := s.removeMembers(ctx, w, lateIDs); err != nil {
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
	w.event(lifecycle.UserDeleted(u.ID))
	if err := w.commit(ctx); err != nil {
		return false, op.Fail(err)
	}
	op.OK("deleted")
	return true, nil
}
