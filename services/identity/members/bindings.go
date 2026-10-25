package members

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/nodes"
)

// Binding is a role for a member on one node of its tree, as listed at a
// node: Inherited when it sits on an ancestor of that node.
type Binding struct {
	ID        string
	MemberID  string
	NodeID    string
	Role      string
	Inherited bool
	Guest     bool
	Principal string
	Name      *string
	CreatedAt time.Time
	UpdatedAt time.Time
	// nodeKind is the kind of the node it sits on, which decides whether its
	// role counts.
	nodeKind string
}

// bindingSelect reads Bindings as listed at node $1; where uses $2 on.
func bindingSelect(where string) string {
	return `
		SELECT b.id, b.member_id, b.node_id, b.role, b.node_id <> $1, ` + guestSQL + `, m.principal, p.name,
		       b.created_at, b.updated_at, bn.kind
		FROM bindings b
		JOIN nodes bn ON bn.id = b.node_id
		JOIN members m ON m.id = b.member_id
		LEFT JOIN principals p ON p.id = m.principal_id
		WHERE b.node_id IN (SELECT ancestor_id FROM node_ancestors WHERE node_id = $1) AND ` + where
}

func scanBinding(row dbx.Scannable) (*Binding, error) {
	var b Binding
	if err := row.Scan(&b.ID, &b.MemberID, &b.NodeID, &b.Role, &b.Inherited, &b.Guest, &b.Principal, &b.Name,
		&b.CreatedAt, &b.UpdatedAt, &b.nodeKind); err != nil {
		return nil, err
	}
	return &b, nil
}

func collectBindings(rows pgx.Rows) ([]*Binding, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Binding, error) { return scanBinding(r) })
}

// insertBinding binds role for a member on a node. One binding per member per
// node: a second is 409.
func (s *Store) insertBinding(ctx context.Context, w *write, memberID, nodeID, role string) (*Binding, error) {
	bindingID := id.New()
	now := s.now().UTC()
	_, err := w.tx.Exec(ctx, `
		INSERT INTO bindings (id, member_id, node_id, role, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $5)
	`, bindingID, memberID, nodeID, role, now)
	if dbx.IsUniqueViolation(err) {
		return nil, errors.ConflictError("This member already has a role here.", "member_id", memberID)
	}
	if err != nil {
		return nil, err
	}
	return scanBinding(w.tx.QueryRow(ctx, bindingSelect(`b.id = $2`), nodeID, bindingID))
}

// CreateBinding binds role for a member already in the node's tree.
func (s *Store) CreateBinding(ctx context.Context, snap *feed.Snapshot, nodeID, memberID, role string) (*Binding, error) {
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
	if err := checkRole(snap, role, n.Kind, "role"); err != nil {
		return nil, op.Expected("invalid", err)
	}
	if err := s.checkInTree(ctx, w, n, memberID); err != nil {
		return nil, op.Reject(err)
	}
	b, err := s.insertBinding(ctx, w, memberID, n.ID, role)
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
func (s *Store) checkInTree(ctx context.Context, w *write, n *nodes.Node, memberID string) error {
	m, err := s.lockedMember(ctx, w, memberID)
	if err == ErrNotFound || (err == nil && (m.removed || m.rootID != n.RootID)) {
		var rootKind string
		if err := w.tx.QueryRow(ctx, `SELECT kind FROM nodes WHERE id = $1`, n.RootID).Scan(&rootKind); err != nil {
			return err
		}
		return errors.FieldError("member_id", fmt.Sprintf("Add this person to the %s first, or invite them.", rootKind))
	}
	return err
}

// ListBindings is the access list: every binding that counts at a node, on it
// and on each ancestor, or with inherited false only its own. Binding id
// ascending.
func (s *Store) ListBindings(ctx context.Context, nodeID string, inherited bool, limit int, startingAfter string) ([]*Binding, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_bindings", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if _, err := s.nodes.Get(ctx, nodeID); err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	rows, err := s.pool.Query(ctx, bindingSelect(`($2 OR b.node_id = $1) AND b.id > $3 ORDER BY b.id LIMIT $4`),
		nodeID, inherited, startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := collectBindings(rows)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := page(out, limit)
	op.OK("ok")
	return out, hasMore, nil
}

// PatchBinding changes a binding's role. It returns the binding before and
// after.
func (s *Store) PatchBinding(ctx context.Context, snap *feed.Snapshot, nodeID, bindingID, role string) (*Binding, *Binding, error) {
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
	if err := checkRole(snap, role, n.Kind, "role"); err != nil {
		return nil, nil, op.Expected("invalid", err)
	}
	if _, err := w.tx.Exec(ctx, `
		UPDATE bindings SET role = $2, updated_at = CASE WHEN role = $2 THEN updated_at ELSE $3 END WHERE id = $1
	`, before.ID, role, s.now().UTC()); err != nil {
		return nil, nil, op.Fail(err)
	}
	after, err := scanBinding(w.tx.QueryRow(ctx, bindingSelect(`b.id = $2`), n.ID, before.ID))
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("ok")
	return before, after, nil
}

// lockedBinding reads a binding sitting on nodeID.
func (s *Store) lockedBinding(ctx context.Context, w *write, nodeID, bindingID string) (*Binding, error) {
	if !id.Valid(bindingID) {
		return nil, ErrNotFound
	}
	b, err := scanBinding(w.tx.QueryRow(ctx, bindingSelect(`b.id = $2 AND b.node_id = $1 FOR UPDATE OF b`), nodeID, bindingID))
	if dbx.IsNoRows(err) {
		return nil, ErrNotFound
	}
	return b, err
}

// DeleteBinding removes a binding. A person's member left with no bindings is
// removed; a service account's isn't. A binding already gone is nil.
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
	if err == ErrNotFound {
		op.OK("unchanged")
		return nil, nil
	}
	if err != nil {
		return nil, op.Fail(err)
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

// Membership is what one member holds at a node: the bindings that count for
// it there, and the permissions they grant.
type Membership struct {
	MemberID    string
	Bindings    []*Binding
	Permissions []string
}

// Membership is GET /nodes/{node_id}/memberships/{member_id}. The member must
// be in the node's tree; a removed one holds nothing.
func (s *Store) Membership(ctx context.Context, snap *feed.Snapshot, nodeID, memberID string) (*Membership, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_membership", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	n, err := s.nodes.Get(ctx, nodeID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	m, err := s.getMember(ctx, s.pool, memberID)
	if err == nil && m.RootID != n.RootID {
		err = ErrNotFound
	}
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	rows, err := s.pool.Query(ctx, bindingSelect(`b.member_id = $2 ORDER BY b.id`), n.ID, m.ID)
	if err != nil {
		return nil, op.Fail(err)
	}
	bindings, err := collectBindings(rows)
	if err != nil {
		return nil, op.Fail(err)
	}
	var held []string
	for _, b := range bindings {
		if r, ok := snap.Role(b.Role); ok && r.BindableAtKind(b.nodeKind) {
			held = append(held, b.Role)
		}
	}
	op.OK("ok")
	return &Membership{MemberID: m.ID, Bindings: bindings, Permissions: Resolve(snap, held).List()}, nil
}
