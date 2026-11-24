package members

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/access"
	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/nodes"
)

// Binding is a role for a member, group, or public principal on one node of its
// tree, as listed at a node: Inherited when it sits on an ancestor of that node.
type Binding struct {
	ID              string
	MemberID        *string
	GroupID         *string
	PublicPrincipal *string
	NodeID          string
	Role            string
	Inherited       bool
	Guest           bool
	Principal       string
	Name            *string
	ExpiresAt       *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// nodeKind is the kind of the node it sits on, which decides whether its
	// role counts.
	nodeKind string
}

// BindingPatch is what PATCH /nodes/{node_id}/bindings/{binding_id} changes.
type BindingPatch struct {
	Role         *string
	ExpiresAtSet bool
	ExpiresAt    *time.Time
}

// bindingSelect reads Bindings as listed at node $1; where uses $2 on.
func bindingSelect(where string) string {
	return `
		SELECT b.id, b.member_id, b.group_id, b.public_principal, b.node_id, b.role, b.node_id <> $1,
		       COALESCE(` + guestSQL + `, false),
		       CASE
		           WHEN b.group_id IS NOT NULL THEN 'group'
		           WHEN b.public_principal IS NOT NULL THEN b.public_principal
		           ELSE m.principal
		       END,
		       CASE
		           WHEN b.group_id IS NOT NULL THEN g.name
		           WHEN b.public_principal IS NOT NULL THEN NULL
		           ELSE p.name
		       END,
		       b.expires_at, b.created_at, b.updated_at, bn.kind
		FROM bindings b
		JOIN nodes bn ON bn.id = b.node_id
		LEFT JOIN members m ON m.id = b.member_id
		LEFT JOIN principals p ON p.id = m.principal_id
		LEFT JOIN groups g ON g.id = b.group_id
		WHERE b.node_id IN (SELECT ancestor_id FROM node_ancestors WHERE node_id = $1) AND ` + where
}

func scanBinding(row dbx.Scannable) (*Binding, error) {
	var b Binding
	if err := row.Scan(&b.ID, &b.MemberID, &b.GroupID, &b.PublicPrincipal, &b.NodeID, &b.Role, &b.Inherited, &b.Guest, &b.Principal, &b.Name,
		&b.ExpiresAt, &b.CreatedAt, &b.UpdatedAt, &b.nodeKind); err != nil {
		return nil, err
	}
	return &b, nil
}

func collectBindings(rows pgx.Rows) ([]*Binding, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Binding, error) { return scanBinding(r) })
}

// insertBinding binds role for a member on a node. Multiple distinct roles per
// member per node are allowed; repeating the same role on the node is 409.
func (s *Store) insertBinding(ctx context.Context, w *write, memberID, nodeID, role string, expiresAt *time.Time) (*Binding, error) {
	bindingID := id.New()
	now := s.now().UTC()
	_, err := w.tx.Exec(ctx, `
		INSERT INTO bindings (id, member_id, node_id, role, expires_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $6)
	`, bindingID, memberID, nodeID, role, expiresAt, now)
	if dbx.IsUniqueViolation(err) {
		return nil, errors.ConflictError("This member already has that role here.", "role", role)
	}
	if err != nil {
		return nil, err
	}
	w.invalidate(lifecycle.InvalidateMember(memberID))
	return scanBinding(w.tx.QueryRow(ctx, bindingSelect(`b.id = $2`), nodeID, bindingID))
}

// insertGroupBinding binds role for a group on a node. Multiple distinct roles
// per group per node are allowed; repeating the same role on the node is 409.
func (s *Store) insertGroupBinding(ctx context.Context, w *write, groupID, nodeID, role string, expiresAt *time.Time) (*Binding, error) {
	bindingID := id.New()
	now := s.now().UTC()
	_, err := w.tx.Exec(ctx, `
		INSERT INTO bindings (id, group_id, node_id, role, expires_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $6)
	`, bindingID, groupID, nodeID, role, expiresAt, now)
	if dbx.IsUniqueViolation(err) {
		return nil, errors.ConflictError("This group already has that role here.", "role", role)
	}
	if err != nil {
		return nil, err
	}
	return scanBinding(w.tx.QueryRow(ctx, bindingSelect(`b.id = $2`), nodeID, bindingID))
}

// CreateGroupBinding binds role for a group belonging to the node's tree.
func (s *Store) CreateGroupBinding(ctx context.Context, snap *feed.Snapshot, nodeID, groupID, role string, expiresAt *time.Time) (*Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_group_binding", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if expiresAt != nil && !expiresAt.After(s.now().UTC()) {
		return nil, op.Expected("invalid", errors.FieldError("expires_at", "Expiry must be in the future."))
	}
	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedWritable(ctx, w.tx, nodeID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if err := checkRoleTx(ctx, w.tx, snap, n.RootID, role, n.Kind, "role"); err != nil {
		return nil, op.Expected("invalid", err)
	}
	if err := s.checkGroupInTree(ctx, w, n, groupID); err != nil {
		return nil, op.Reject(err)
	}
	b, err := s.insertGroupBinding(ctx, w, groupID, n.ID, role, expiresAt)
	if err != nil {
		return nil, op.Reject(err)
	}
	w.invalidate(lifecycle.InvalidateRoot(n.RootID))
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("created")
	return b, nil
}

// CreatePublicBinding binds role for a public principal (all_users or
// all_authenticated_users) on a node whose kind sets allow_public: true.
func (s *Store) CreatePublicBinding(ctx context.Context, snap *feed.Snapshot, nodeID, publicPrincipal, role string, expiresAt *time.Time) (*Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_public_binding", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if publicPrincipal != PrincipalAllUsers && publicPrincipal != PrincipalAllAuthenticatedUsers {
		return nil, op.Expected("invalid", errors.FieldError("public_principal", "Must be all_users or all_authenticated_users."))
	}
	if expiresAt != nil && !expiresAt.After(s.now().UTC()) {
		return nil, op.Expected("invalid", errors.FieldError("expires_at", "Expiry must be in the future."))
	}
	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedWritable(ctx, w.tx, nodeID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if k, ok := snap.Kind(n.Kind); !ok || !k.AllowPublic {
		return nil, op.Expected("not allowed", errors.FieldError("public_principal", fmt.Sprintf("Public bindings are not allowed on %s.", n.Kind)))
	}
	if err := checkRoleTx(ctx, w.tx, snap, n.RootID, role, n.Kind, "role"); err != nil {
		return nil, op.Expected("invalid", err)
	}
	bindingID := id.New()
	now := s.now().UTC()
	_, err = w.tx.Exec(ctx, `
		INSERT INTO bindings (id, public_principal, node_id, role, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
	`, bindingID, publicPrincipal, n.ID, role, expiresAt, now)
	if dbx.IsUniqueViolation(err) {
		return nil, op.Expected("conflict", errors.ConflictError("This public principal already has that role here.", "role", role))
	}
	if err != nil {
		return nil, op.Fail(err)
	}
	b, err := scanBinding(w.tx.QueryRow(ctx, bindingSelect(`b.id = $2`), n.ID, bindingID))
	if err != nil {
		return nil, op.Fail(err)
	}
	w.invalidate(lifecycle.InvalidateRoot(n.RootID))
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("created")
	return b, nil
}

// CreateBinding binds role for a member already in the node's tree.
func (s *Store) CreateBinding(ctx context.Context, snap *feed.Snapshot, nodeID, memberID, role string, expiresAt *time.Time) (*Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_binding", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if expiresAt != nil && !expiresAt.After(s.now().UTC()) {
		return nil, op.Expected("invalid", errors.FieldError("expires_at", "Expiry must be in the future."))
	}
	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedWritable(ctx, w.tx, nodeID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if err := checkRoleTx(ctx, w.tx, snap, n.RootID, role, n.Kind, "role"); err != nil {
		return nil, op.Expected("invalid", err)
	}
	if err := s.checkInTree(ctx, w, n, memberID); err != nil {
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

// PatchBinding changes a binding's role and/or expires_at. It returns the
// binding before and after.
func (s *Store) PatchBinding(ctx context.Context, snap *feed.Snapshot, nodeID, bindingID string, patch BindingPatch) (*Binding, *Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "patch_binding", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if patch.Role == nil && !patch.ExpiresAtSet {
		return nil, nil, op.Expected("invalid", errors.FieldError("", "Nothing to update."))
	}
	if patch.ExpiresAtSet && patch.ExpiresAt != nil && !patch.ExpiresAt.After(s.now().UTC()) {
		return nil, nil, op.Expected("invalid", errors.FieldError("expires_at", "Expiry must be in the future."))
	}

	w, err := s.begin(ctx)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	n, err := s.nodes.LockedWritable(ctx, w.tx, nodeID)
	if err != nil {
		return nil, nil, op.Reject(err, nodes.ErrNotFound)
	}
	before, err := s.lockedBinding(ctx, w, n.ID, bindingID)
	if err != nil {
		return nil, nil, op.Reject(err, ErrNotFound)
	}
	role := before.Role
	if patch.Role != nil {
		if err := checkRoleTx(ctx, w.tx, snap, n.RootID, *patch.Role, n.Kind, "role"); err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
		role = *patch.Role
	}
	expiresAt := before.ExpiresAt
	if patch.ExpiresAtSet {
		expiresAt = patch.ExpiresAt
	}
	changed := before.Role != role || !timesEqual(before.ExpiresAt, expiresAt)
	if _, err := w.tx.Exec(ctx, `
		UPDATE bindings SET role = $2, expires_at = $3, updated_at = CASE WHEN $4 THEN $5 ELSE updated_at END WHERE id = $1
	`, before.ID, role, expiresAt, changed, s.now().UTC()); err != nil {
		if dbx.IsUniqueViolation(err) {
			msg := "This member already has that role here."
			if before.GroupID != nil {
				msg = "This group already has that role here."
			} else if before.PublicPrincipal != nil {
				msg = "This public principal already has that role here."
			}
			return nil, nil, op.Expected("conflict", errors.ConflictError(msg, "role", role))
		}
		return nil, nil, op.Fail(err)
	}
	after, err := scanBinding(w.tx.QueryRow(ctx, bindingSelect(`b.id = $2`), n.ID, before.ID))
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	if changed {
		if before.MemberID != nil {
			w.invalidate(lifecycle.InvalidateMember(*before.MemberID))
		} else if before.GroupID != nil || before.PublicPrincipal != nil {
			w.invalidate(lifecycle.InvalidateRoot(n.RootID))
		}
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("ok")
	return before, after, nil
}

func timesEqual(a, b *time.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Equal(*b)
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
	if b.MemberID != nil {
		w.invalidate(lifecycle.InvalidateMember(*b.MemberID))
		if err := s.removeEmptyPeople(ctx, w, []string{*b.MemberID}); err != nil {
			return nil, op.Fail(err)
		}
	} else if b.GroupID != nil || b.PublicPrincipal != nil {
		w.invalidate(lifecycle.InvalidateRoot(n.RootID))
	}
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("deleted")
	return b, nil
}

// Membership is what one member holds at a node: the bindings that count for
// it there (both direct and through groups), and the permissions they grant.
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
	rows, err := s.pool.Query(ctx, bindingSelect(`(
		b.member_id = $2
		OR EXISTS (SELECT 1 FROM group_members gm WHERE gm.group_id = b.group_id AND gm.member_id = $2)
	) ORDER BY b.id`), n.ID, m.ID)
	if err != nil {
		return nil, op.Fail(err)
	}
	bindings, err := collectBindings(rows)
	if err != nil {
		return nil, op.Fail(err)
	}
	var held []string
	var customIDs []string
	customKinds := map[string]map[string]bool{}
	now := s.now().UTC()
	if !m.Removed && !m.Suspended {
		for _, b := range bindings {
			if b.ExpiresAt != nil && !b.ExpiresAt.After(now) {
				continue
			}
			if r, ok := snap.Role(b.Role); ok {
				if r.BindableAtKind(b.nodeKind) {
					held = append(held, b.Role)
				}
			} else if id.Valid(b.Role) {
				if customKinds[b.Role] == nil {
					customKinds[b.Role] = map[string]bool{}
					customIDs = append(customIDs, b.Role)
				}
				customKinds[b.Role][b.nodeKind] = true
			}
		}
	}
	var customPerms []string
	if len(customIDs) > 0 {
		crows, err := s.pool.Query(ctx, `
			SELECT id, bindable_at, permissions FROM org_roles WHERE root_id = $1 AND id = ANY($2)
		`, n.RootID, customIDs)
		if err != nil {
			return nil, op.Fail(err)
		}
		var cid string
		var bindableAt, perms []string
		if _, err := pgx.ForEachRow(crows, []any{&cid, &bindableAt, &perms}, func() error {
			for _, k := range bindableAt {
				if customKinds[cid][k] {
					customPerms = append(customPerms, perms...)
					break
				}
			}
			return nil
		}); err != nil {
			return nil, op.Fail(err)
		}
	}
	op.OK("ok")
	return &Membership{MemberID: m.ID, Bindings: bindings, Permissions: access.ResolveWithCustom(snap, held, customPerms).List()}, nil
}
