package members

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/nodes"
)

// Member is one principal's presence in one tree.
type Member struct {
	ID        string
	RootID    string
	Principal string
	Name      *string
	Removed   bool
	// Guest is an active member outside the directory with no live binding at
	// its root (docs/concepts/model.md). Derived, not stored.
	Guest bool
	// PrincipalStatus is active or suspended; empty once the principal is gone.
	PrincipalStatus string
	CreatedAt       time.Time
}

// Status is the member's status on the wire: active or removed.
func (m *Member) Status() string {
	if m.Removed {
		return StatusRemoved
	}
	return StatusActive
}

// memberSelect reads Members. $1 is the sign-in node's id, $2 now; where may
// use $3 on.
func memberSelect(where string) string {
	return `
		SELECT m.id, m.root_id, m.principal, p.name, m.removed_at IS NOT NULL,
		       (m.removed_at IS NULL AND m.root_id <> $1 AND NOT EXISTS (
		           SELECT 1 FROM bindings b WHERE b.member_id = m.id AND b.node_id = m.root_id
		             AND (b.expires_at IS NULL OR b.expires_at > $2))),
		       COALESCE(p.status, ''), m.created_at
		FROM members m LEFT JOIN principals p ON p.id = m.principal_id
		WHERE ` + where
}

func scanMember(row dbx.Scannable) (*Member, error) {
	var m Member
	err := row.Scan(&m.ID, &m.RootID, &m.Principal, &m.Name, &m.Removed, &m.Guest, &m.PrincipalStatus, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) getMember(ctx context.Context, q dbx.Querier, memberID string) (*Member, error) {
	if !id.Valid(id.Member, memberID) {
		return nil, ErrNotFound
	}
	m, err := scanMember(q.QueryRow(ctx, memberSelect(`m.id = $3`), s.signInID, s.now().UTC(), memberID))
	if dbx.IsNoRows(err) {
		return nil, ErrNotFound
	}
	return m, err
}

// ensurePerson finds the person behind an IdP subject, or creates them with
// their directory member. The caller holds the sign-in tree's lock.
func (s *Store) ensurePerson(ctx context.Context, w *write, issuer, subject string) (string, error) {
	var principalID string
	err := w.tx.QueryRow(ctx, `SELECT principal_id FROM idp_links WHERE issuer = $1 AND subject = $2`, issuer, subject).Scan(&principalID)
	if err == nil {
		return principalID, nil
	}
	if !dbx.IsNoRows(err) {
		return "", err
	}
	now := s.now().UTC()
	principalID = id.New("")
	if _, err := w.tx.Exec(ctx, `
		INSERT INTO principals (id, kind, created_at, updated_at) VALUES ($1, 'person', $2, $2)
	`, principalID, now); err != nil {
		return "", err
	}
	if _, err := w.tx.Exec(ctx, `
		INSERT INTO idp_links (issuer, subject, principal_id, created_at) VALUES ($1, $2, $3, $4)
	`, issuer, subject, principalID, now); err != nil {
		return "", err
	}
	if _, err := s.insertMember(ctx, w, principalID, PrincipalPerson, s.signInID); err != nil {
		return "", err
	}
	return principalID, nil
}

// lockedRoot locks a root's tree (and alsoLock's) and checks it's a root other
// than the sign-in node: the only nodes with a members API.
func (s *Store) lockedRoot(ctx context.Context, w *write, rootID string, alsoLock ...string) (*nodes.Node, error) {
	n, err := s.nodes.LockedNode(ctx, w.tx, rootID, alsoLock...)
	if err != nil {
		return nil, err
	}
	if n.ParentID != nil || n.ID == s.signInID {
		return nil, nodes.ErrNotFound
	}
	return n, nil
}

func (s *Store) checkRoot(ctx context.Context, rootID string) error {
	n, err := s.nodes.Get(ctx, rootID)
	if err != nil {
		return err
	}
	if n.ParentID != nil || n.ID == s.signInID {
		return nodes.ErrNotFound
	}
	return nil
}

// AddMemberInput is POST /nodes/{root_id}/members.
type AddMemberInput struct {
	Issuer, Subject string
	Role            *string
	ExpiresAt       *time.Time
}

// AddMember makes the person behind an IdP subject a member of a root, with
// role (or default_role) bound at the root. The person and their directory
// member are created if the subject is new.
func (s *Store) AddMember(ctx context.Context, snap *feed.Snapshot, rootID string, in AddMemberInput) (*Member, *Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "add_member", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	issuer, subject := strings.TrimSpace(in.Issuer), strings.TrimSpace(in.Subject)
	if issuer == "" || subject == "" {
		return nil, nil, op.Expected("invalid", errors.FieldError("idp", "Choose someone to add."))
	}
	w, err := s.begin(ctx)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	root, err := s.lockedRoot(ctx, w, rootID, s.signInID)
	if err != nil {
		return nil, nil, op.Reject(err, nodes.ErrNotFound)
	}
	role := snap.DefaultRole
	if in.Role != nil {
		role = *in.Role
	}
	if err := s.checkRole(snap, role, root.Kind, in.ExpiresAt, "expires_at"); err != nil {
		return nil, nil, op.Expected("invalid", err)
	}
	principalID, err := s.ensurePerson(ctx, w, issuer, subject)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	existing, err := activeMemberID(ctx, w.tx, principalID, root.ID)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	if existing != "" {
		return nil, nil, op.Expected("exists", errors.ConflictError("This person is already a member.", "member", existing))
	}
	memberID, err := s.insertMember(ctx, w, principalID, PrincipalPerson, root.ID)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	b, err := s.insertBinding(ctx, w, memberID, root.ID, role, in.ExpiresAt)
	if err != nil {
		return nil, nil, op.Reject(err)
	}
	m, err := s.getMember(ctx, w.tx, memberID)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("added")
	return m, b, nil
}

// ListMembers is a root's active members, guests marked, id ascending.
func (s *Store) ListMembers(ctx context.Context, rootID string, limit int, startingAfter string) ([]*Member, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_members", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if err := s.checkRoot(ctx, rootID); err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	rows, err := s.pool.Query(ctx, memberSelect(`m.root_id = $3 AND m.removed_at IS NULL AND m.id > $4 ORDER BY m.id LIMIT $5`),
		s.signInID, s.now().UTC(), rootID, startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Member, error) { return scanMember(r) })
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := page(out, limit)
	op.OK("ok")
	return out, hasMore, nil
}

// GetMember is a member of a root's tree, removed ones included.
func (s *Store) GetMember(ctx context.Context, rootID, memberID string) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_member", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if err := s.checkRoot(ctx, rootID); err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	m, err := s.getMember(ctx, s.pool, memberID)
	if err == nil && m.RootID != rootID {
		err = ErrNotFound
	}
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	op.OK("ok")
	return m, nil
}

// RemoveMember removes a member from a root's tree: bindings deleted,
// member.removed. Final. A member already removed is left as it is: changed
// is false. A service account's member goes only with the service account.
func (s *Store) RemoveMember(ctx context.Context, rootID, memberID string) (bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "remove_member", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return false, op.Fail(err)
	}
	defer w.rollback(ctx)
	root, err := s.lockedRoot(ctx, w, rootID)
	if err != nil {
		return false, op.Reject(err, nodes.ErrNotFound)
	}
	if !id.Valid(id.Member, memberID) {
		return false, op.Expected("not found", ErrNotFound)
	}
	m, err := s.lockedMember(ctx, w, memberID)
	if err == nil && m.rootID != root.ID {
		err = ErrNotFound
	}
	if err != nil {
		return false, op.Reject(err, ErrNotFound)
	}
	if m.principal == PrincipalServiceAccount {
		return false, op.Expected("service account", errors.FieldError("member_id", "Delete the service account to remove it."))
	}
	if m.removed {
		op.OK("unchanged")
		return false, nil
	}
	if err := s.removeMember(ctx, w, m); err != nil {
		return false, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return false, op.Fail(err)
	}
	op.OK("removed")
	return true, nil
}

// --- the directory member: the person ---

// directoryMember reads a directory member, or ErrNotFound for any other.
func (s *Store) directoryMember(ctx context.Context, q dbx.Querier, memberID string) (*Member, error) {
	m, err := s.getMember(ctx, q, memberID)
	if err != nil {
		return nil, err
	}
	if m.RootID != s.signInID {
		return nil, ErrNotFound
	}
	return m, nil
}

// personPrincipal is the principal of an active directory member.
func (s *Store) personPrincipal(ctx context.Context, q dbx.Querier, memberID string) (string, error) {
	if !id.Valid(id.Member, memberID) {
		return "", ErrNotFound
	}
	var principalID string
	err := q.QueryRow(ctx, `
		SELECT principal_id FROM members
		WHERE id = $1 AND root_id = $2 AND removed_at IS NULL AND principal_id IS NOT NULL
	`, memberID, s.signInID).Scan(&principalID)
	if dbx.IsNoRows(err) {
		return "", ErrNotFound
	}
	return principalID, err
}

// Person is GET /members/{member_id}: the person behind a directory member.
func (s *Store) Person(ctx context.Context, memberID string) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_person", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	m, err := s.directoryMember(ctx, s.pool, memberID)
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	op.OK("ok")
	return m, nil
}

// PersonStatus is a directory member's status on GET /members/{member_id}:
// active, suspended, or removed for a deleted account.
func PersonStatus(m *Member) string {
	if m.Removed || m.PrincipalStatus == "" {
		return StatusRemoved
	}
	return m.PrincipalStatus
}

// SetStatus suspends or reactivates a person. It returns the status before.
func (s *Store) SetStatus(ctx context.Context, memberID, status string) (string, *Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "set_person_status", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if status != StatusActive && status != StatusSuspended {
		return "", nil, op.Expected("invalid", errors.FieldError("status", errors.FallbackValidation))
	}
	w, err := s.begin(ctx)
	if err != nil {
		return "", nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	principalID, err := s.personPrincipal(ctx, w.tx, memberID)
	if err != nil {
		return "", nil, op.Reject(err, ErrNotFound)
	}
	var before string
	if err := w.tx.QueryRow(ctx, `SELECT status FROM principals WHERE id = $1 FOR UPDATE`, principalID).Scan(&before); err != nil {
		return "", nil, op.Fail(err)
	}
	if before != status {
		if _, err := w.tx.Exec(ctx, `UPDATE principals SET status = $2, updated_at = $3 WHERE id = $1`, principalID, status, s.now().UTC()); err != nil {
			return "", nil, op.Fail(err)
		}
	}
	m, err := s.directoryMember(ctx, w.tx, memberID)
	if err != nil {
		return "", nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return "", nil, op.Fail(err)
	}
	op.OK("ok")
	return before, m, nil
}

// DeleteAccount deletes a person (docs/concepts/lifecycle.md#deleting-an-account):
// every member they have is removed, with member.removed per tree, and the
// principal goes with its IdP links and personal keys. Removed member rows
// stay, with no name. A deleted account is left as it is: changed is false.
func (s *Store) DeleteAccount(ctx context.Context, memberID string) (bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_account", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	m, err := s.directoryMember(ctx, s.pool, memberID)
	if err != nil {
		return false, op.Reject(err, ErrNotFound)
	}
	if m.Removed {
		op.OK("unchanged")
		return false, nil
	}
	w, err := s.begin(ctx)
	if err != nil {
		return false, op.Fail(err)
	}
	defer w.rollback(ctx)
	principalID, err := s.personPrincipal(ctx, w.tx, memberID)
	if err != nil {
		return false, op.Reject(err, ErrNotFound)
	}
	rows, err := w.tx.Query(ctx, `SELECT DISTINCT root_id FROM members WHERE principal_id = $1 AND removed_at IS NULL`, principalID)
	if err != nil {
		return false, op.Fail(err)
	}
	roots, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, op.Fail(err)
	}
	if err := nodes.LockTrees(ctx, w.tx, append(roots, s.signInID)); err != nil {
		return false, op.Fail(err)
	}
	rows, err = w.tx.Query(ctx, `
		SELECT id FROM members WHERE principal_id = $1 AND removed_at IS NULL ORDER BY id
	`, principalID)
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
	if _, err := w.tx.Exec(ctx, `DELETE FROM principals WHERE id = $1`, principalID); err != nil {
		return false, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return false, op.Fail(err)
	}
	op.OK("deleted")
	return true, nil
}

// page trims a limit+1 read to limit and says whether there was more.
func page[T any](items []T, limit int) ([]T, bool) {
	if len(items) > limit {
		return items[:limit], true
	}
	return items, false
}
