package members

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/nodes"
)

// Binding is a role for a member on one node of its tree.
type Binding struct {
	ID        string
	MemberID  string
	NodeID    string
	Role      string
	ExpiresAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

const bindingColumns = `id, member_id, node_id, role, expires_at, created_at, updated_at`

func scanBinding(row dbx.Scannable) (*Binding, error) {
	var b Binding
	if err := row.Scan(&b.ID, &b.MemberID, &b.NodeID, &b.Role, &b.ExpiresAt, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, err
	}
	return &b, nil
}

// insertBinding binds role for a member on a node. One binding per member per
// node: a second is 409.
func (s *Store) insertBinding(ctx context.Context, w *write, memberID, nodeID, role string, expiresAt *time.Time) (*Binding, error) {
	now := s.now().UTC()
	b, err := scanBinding(w.tx.QueryRow(ctx, `
		INSERT INTO bindings (id, member_id, node_id, role, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		RETURNING `+bindingColumns, id.New(id.Binding), memberID, nodeID, role, expiresAt, now))
	if dbx.IsUniqueViolation(err) {
		return nil, errors.ConflictError("This member already has a role here.", "member", memberID)
	}
	return b, err
}

// CreateBinding binds role for a member already in the node's tree.
func (s *Store) CreateBinding(ctx context.Context, snap *feed.Snapshot, nodeID, memberID, role string, expiresAt *time.Time) (*Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_binding", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedNode(ctx, w.tx, nodeID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if err := s.checkRole(snap, role, n.Kind, expiresAt, "expires_at"); err != nil {
		return nil, op.Expected("invalid", err)
	}
	if err := s.checkInTree(ctx, w, snap, n, memberID); err != nil {
		return nil, op.Reject(err)
	}
	b, err := s.insertBinding(ctx, w, memberID, n.ID, role, expiresAt)
	if err != nil {
		return nil, op.Reject(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("created")
	return b, nil
}

// checkInTree refuses a direct binding for anyone not an active member of n's
// tree: guests come through invites.
func (s *Store) checkInTree(ctx context.Context, w *write, snap *feed.Snapshot, n *nodes.Node, memberID string) error {
	notIn := func() error {
		var rootKind string
		if err := w.tx.QueryRow(ctx, `SELECT kind FROM nodes WHERE id = $1`, n.RootID).Scan(&rootKind); err != nil {
			return err
		}
		return errors.FieldError("member", fmt.Sprintf("Add this person to the %s first, or invite them.", nodes.KindWord(snap, rootKind)))
	}
	if !id.Valid(id.Member, memberID) {
		return notIn()
	}
	m, err := s.lockedMember(ctx, w, memberID)
	if err == ErrNotFound || (err == nil && (m.removed || m.rootID != n.RootID)) {
		return notIn()
	}
	return err
}

// ListBindings is the live bindings on a node, id ascending.
func (s *Store) ListBindings(ctx context.Context, nodeID string, limit int, startingAfter string) ([]*Binding, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_bindings", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if _, err := s.nodes.Get(ctx, nodeID); err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+bindingColumns+` FROM bindings
		WHERE node_id = $1 AND (expires_at IS NULL OR expires_at > $2) AND id > $3
		ORDER BY id LIMIT $4
	`, nodeID, s.now().UTC(), startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Binding, error) { return scanBinding(r) })
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := page(out, limit)
	op.OK("ok")
	return out, hasMore, nil
}

// PatchBinding changes a binding's role or expiry. ExpiresAt set with a nil
// value clears it. The result must still meet the role's rules. It returns
// the binding before and after.
func (s *Store) PatchBinding(ctx context.Context, snap *feed.Snapshot, nodeID, bindingID string, role *string, expiresAt httpx.NullableTime) (*Binding, *Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "patch_binding", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedNode(ctx, w.tx, nodeID)
	if err != nil {
		return nil, nil, op.Reject(err, nodes.ErrNotFound)
	}
	before, err := s.lockedBinding(ctx, w, n.ID, bindingID)
	if err != nil {
		return nil, nil, op.Reject(err, ErrNotFound)
	}
	newRole, newExpiry := before.Role, before.ExpiresAt
	if role != nil {
		newRole = *role
	}
	if expiresAt.Set {
		newExpiry = expiresAt.Value
	}
	if err := s.checkRole(snap, newRole, n.Kind, newExpiry, "expires_at"); err != nil {
		return nil, nil, op.Expected("invalid", err)
	}
	after, err := scanBinding(w.tx.QueryRow(ctx, `
		UPDATE bindings SET role = $2, expires_at = $3, updated_at = $4 WHERE id = $1
		RETURNING `+bindingColumns, before.ID, newRole, newExpiry, s.now().UTC()))
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("ok")
	return before, after, nil
}

func (s *Store) lockedBinding(ctx context.Context, w *write, nodeID, bindingID string) (*Binding, error) {
	if !id.Valid(id.Binding, bindingID) {
		return nil, ErrNotFound
	}
	b, err := scanBinding(w.tx.QueryRow(ctx, `
		SELECT `+bindingColumns+` FROM bindings WHERE id = $1 AND node_id = $2 FOR UPDATE
	`, bindingID, nodeID))
	if dbx.IsNoRows(err) {
		return nil, ErrNotFound
	}
	return b, err
}

// DeleteBinding removes a binding. A person's member outside the directory
// left with no bindings is removed; a service account's isn't.
func (s *Store) DeleteBinding(ctx context.Context, nodeID, bindingID string) (*Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_binding", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedNode(ctx, w.tx, nodeID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	b, err := s.lockedBinding(ctx, w, n.ID, bindingID)
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	if _, err := w.tx.Exec(ctx, `DELETE FROM bindings WHERE id = $1`, b.ID); err != nil {
		return nil, op.Fail(err)
	}
	if err := s.removeEmptyPeople(ctx, w, []string{b.MemberID}); err != nil {
		return nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("deleted")
	return b, nil
}

// AccessEntry is one binding that counts at a node.
type AccessEntry struct {
	BindingID string
	MemberID  string
	Principal string
	Name      *string
	Role      string
	NodeID    string
	Inherited bool
	ExpiresAt *time.Time
	Guest     bool
}

// Access is every binding that counts at a node: on it or an ancestor, live,
// with a role bindable where it sits. On a root, guests' bindings below it
// too. Binding id ascending.
func (s *Store) Access(ctx context.Context, snap *feed.Snapshot, nodeID string, limit int, startingAfter string) ([]*AccessEntry, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_access", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	n, err := s.nodes.Get(ctx, nodeID)
	if err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	kinds, roles := bindablePairs(snap)
	rows, err := s.pool.Query(ctx, `
		SELECT id, member_id, principal, name, role, node_id, inherited, expires_at, guest FROM (
			SELECT b.id, b.member_id, m.principal, p.name, b.role, b.node_id,
			       b.node_id <> $1 AND b.node_id IN (SELECT ancestor_id FROM node_ancestors WHERE node_id = $1) AS inherited,
			       b.expires_at,
			       (m.root_id <> $3 AND NOT EXISTS (
			           SELECT 1 FROM bindings rb WHERE rb.member_id = m.id AND rb.node_id = m.root_id
			             AND (rb.expires_at IS NULL OR rb.expires_at > $4))) AS guest,
			       b.node_id IN (SELECT ancestor_id FROM node_ancestors WHERE node_id = $1) AS here
			FROM bindings b
			JOIN nodes bn ON bn.id = b.node_id
			JOIN members m ON m.id = b.member_id AND m.removed_at IS NULL
			LEFT JOIN principals p ON p.id = m.principal_id
			WHERE m.root_id = $2 AND b.id > $5 AND `+counts("$4", "$6", "$7")+`
		) x
		WHERE here OR ($1 = $2 AND guest)
		ORDER BY id LIMIT $8
	`, n.ID, n.RootID, s.signInID, s.now().UTC(), startingAfter, kinds, roles, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*AccessEntry, error) {
		var e AccessEntry
		err := r.Scan(&e.BindingID, &e.MemberID, &e.Principal, &e.Name, &e.Role, &e.NodeID, &e.Inherited, &e.ExpiresAt, &e.Guest)
		return &e, err
	})
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := page(out, limit)
	op.OK("ok")
	return out, hasMore, nil
}
