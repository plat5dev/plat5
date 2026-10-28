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
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/nodes"
)

// Member is one principal's presence in one tree.
type Member struct {
	ID        string
	RootID    string
	Principal string
	Name      *string
	Removed   bool
	// Guest is a person's active member with no binding at its root
	// (docs/concepts/model.md). Derived, not stored.
	Guest     bool
	CreatedAt time.Time
}

// Status is the member's status on the wire: active or removed.
func (m *Member) Status() string {
	if m.Removed {
		return StatusRemoved
	}
	return StatusActive
}

// guestSQL is whether member m is a guest.
const guestSQL = `(m.principal = 'person' AND m.removed_at IS NULL AND NOT EXISTS (
	SELECT 1 FROM bindings gb WHERE gb.member_id = m.id AND gb.node_id = m.root_id))`

// memberSelect reads Members; where uses $1 on.
func memberSelect(where string) string {
	return `
		SELECT m.id, m.root_id, m.principal, p.name, m.removed_at IS NOT NULL, ` + guestSQL + `, m.created_at
		FROM members m LEFT JOIN principals p ON p.id = m.principal_id
		WHERE ` + where
}

func scanMember(row dbx.Scannable) (*Member, error) {
	var m Member
	err := row.Scan(&m.ID, &m.RootID, &m.Principal, &m.Name, &m.Removed, &m.Guest, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) getMember(ctx context.Context, q dbx.Querier, memberID string) (*Member, error) {
	if !id.Valid(memberID) {
		return nil, ErrNotFound
	}
	m, err := scanMember(q.QueryRow(ctx, memberSelect(`m.id = $1`), memberID))
	if dbx.IsNoRows(err) {
		return nil, ErrNotFound
	}
	return m, err
}

// ensurePerson finds the person behind an IdP subject, or creates them with
// a user id and user.created. It holds that subject's lock, so two first
// sign-ins make one person.
func (s *Store) ensurePerson(ctx context.Context, w *write, issuer, subject string) (string, error) {
	if _, err := w.tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2 || ' ' || $3))`, personLockSpace, issuer, subject); err != nil {
		return "", err
	}
	var principalID string
	err := w.tx.QueryRow(ctx, `SELECT principal_id FROM idp_links WHERE issuer = $1 AND subject = $2`, issuer, subject).Scan(&principalID)
	if err == nil {
		return principalID, nil
	}
	if !dbx.IsNoRows(err) {
		return "", err
	}
	now := s.now().UTC()
	principalID, userID := id.New(), id.New()
	if _, err := w.tx.Exec(ctx, `
		INSERT INTO principals (id, kind, user_id, created_at, updated_at) VALUES ($1, 'person', $2, $3, $3)
	`, principalID, userID, now); err != nil {
		return "", err
	}
	if _, err := w.tx.Exec(ctx, `
		INSERT INTO idp_links (issuer, subject, principal_id, created_at) VALUES ($1, $2, $3, $4)
	`, issuer, subject, principalID, now); err != nil {
		return "", err
	}
	w.userEvent(lifecycle.UserCreated(userID))
	return principalID, nil
}

// lockedRoot locks a root's tree and checks it's a root: the only nodes with
// a members API.
func (s *Store) lockedRoot(ctx context.Context, w *write, rootID string) (*nodes.Node, error) {
	n, err := s.nodes.LockedNode(ctx, w.tx, rootID)
	if err != nil {
		return nil, err
	}
	if n.ParentID != nil {
		return nil, nodes.ErrNotFound
	}
	return n, nil
}

func (s *Store) checkRoot(ctx context.Context, rootID string) error {
	n, err := s.nodes.Get(ctx, rootID)
	if err != nil {
		return err
	}
	if n.ParentID != nil {
		return nodes.ErrNotFound
	}
	return nil
}

// AddMemberInput is POST /nodes/{root_id}/members.
type AddMemberInput struct {
	Issuer, Subject string
	Role            string
}

// AddMember makes the person behind an IdP subject a member of a root, with
// role bound at the root. The person is created if the subject is new.
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
	root, err := s.lockedRoot(ctx, w, rootID)
	if err != nil {
		return nil, nil, op.Reject(err, nodes.ErrNotFound)
	}
	if err := checkRole(snap, in.Role, root.Kind, "role"); err != nil {
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
		return nil, nil, op.Expected("exists", errors.ConflictError("This person is already a member.", "member_id", existing))
	}
	memberID, err := s.insertMember(ctx, w, principalID, PrincipalPerson, root.ID)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	b, err := s.insertBinding(ctx, w, memberID, root.ID, in.Role)
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
	rows, err := s.pool.Query(ctx, memberSelect(`m.root_id = $1 AND m.removed_at IS NULL AND m.id > $2 ORDER BY m.id LIMIT $3`),
		rootID, startingAfter, limit+1)
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
