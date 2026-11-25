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

// Binding is a role for a member or group on one node of its tree, as listed
// at a node: Inherited when it sits on an ancestor of that node.
type Binding struct {
	ID        string
	MemberID  *string
	GroupID   *string
	NodeID    string
	Role      string
	Inherited bool
	Guest     bool
	Principal string
	Name      *string
	ExpiresAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BindingPatch is what PATCH /nodes/{node_id}/bindings/{binding_id} changes.
type BindingPatch struct {
	Role         *string
	ExpiresAtSet bool
	ExpiresAt    *time.Time
}

// bindingSelect reads Bindings as listed at node $1; nowParam names the
// timestamp placeholder for guestSQL and where uses $2 on.
func bindingSelect(nowParam, where string) string {
	return `
		SELECT b.id, b.member_id, b.group_id, b.node_id, b.role, b.node_id <> $1,
		       COALESCE(` + guestSQL(nowParam) + `, false),
		       CASE
		           WHEN b.group_id IS NOT NULL THEN 'group'
		           ELSE m.principal
		       END,
		       CASE
		           WHEN b.group_id IS NOT NULL THEN g.name
		           ELSE p.name
		       END,
		       b.expires_at, b.created_at, b.updated_at
		FROM bindings b
		LEFT JOIN members m ON m.id = b.member_id
		LEFT JOIN principals p ON p.id = m.principal_id
		LEFT JOIN groups g ON g.id = b.group_id
		WHERE b.node_id IN (SELECT ancestor_id FROM node_ancestors WHERE node_id = $1) AND ` + where
}

func scanBinding(row dbx.Scannable) (*Binding, error) {
	var b Binding
	if err := row.Scan(&b.ID, &b.MemberID, &b.GroupID, &b.NodeID, &b.Role, &b.Inherited, &b.Guest, &b.Principal, &b.Name,
		&b.ExpiresAt, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, err
	}
	return &b, nil
}

func collectBindings(rows pgx.Rows) ([]*Binding, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Binding, error) { return scanBinding(r) })
}

// insertBinding binds role for a member on a node. Multiple distinct roles per
// member per node are allowed; repeating an unexpired role on the node is 409.
func (s *Store) insertBinding(ctx context.Context, w *write, memberID, nodeID, role string, expiresAt *time.Time) (*Binding, error) {
	b, err := s.insertSubjectBinding(ctx, w, "member_id", memberID, nodeID, role, expiresAt, "This member already has that role here.")
	if err != nil {
		return nil, err
	}
	w.invalidate(lifecycle.InvalidateMember(memberID))
	return b, nil
}

// insertSubjectBinding inserts a binding for a member or group, replacing any
// expired binding for the same (subject, node, role) so re-granting succeeds.
func (s *Store) insertSubjectBinding(ctx context.Context, w *write, col, subject, nodeID, role string, expiresAt *time.Time, conflictMsg string) (*Binding, error) {
	bindingID := id.New()
	now := s.now().UTC()
	if _, err := w.tx.Exec(ctx, `
		DELETE FROM bindings WHERE `+col+` = $1 AND node_id = $2 AND role = $3 AND expires_at IS NOT NULL AND expires_at <= $4
	`, subject, nodeID, role, now); err != nil {
		return nil, err
	}
	_, err := w.tx.Exec(ctx, `
		INSERT INTO bindings (id, `+col+`, node_id, role, expires_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $6)
	`, bindingID, subject, nodeID, role, expiresAt, now)
	if dbx.IsUniqueViolation(err) {
		return nil, errors.ConflictError(conflictMsg, "role", role)
	}
	if err != nil {
		return nil, err
	}
	return scanBinding(w.tx.QueryRow(ctx, bindingSelect("$3", `b.id = $2`), nodeID, bindingID, now))
}

// createBinding runs the shared transaction/validation flow for creating a
// member or group binding on nodeID.
func (s *Store) createBinding(
	ctx context.Context,
	opName string,
	snap *feed.Snapshot,
	nodeID, role string,
	expiresAt *time.Time,
	insert func(ctx context.Context, w *write, n *nodes.Node) (*Binding, error),
) (*Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, opName, dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if snap.ExternalAuthz() {
		return nil, op.Expected("disabled", errBuiltInRBACDisabled())
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
	if err := checkRoleTx(ctx, w.tx, snap, n.RootID, role, n.Kind, "role"); err != nil {
		return nil, op.Expected("invalid", err)
	}
	b, err := insert(ctx, w, n)
	if err != nil {
		return nil, op.Reject(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("created")
	return b, nil
}

// CreateGroupBinding binds role for a group belonging to the node's tree.
func (s *Store) CreateGroupBinding(ctx context.Context, snap *feed.Snapshot, nodeID, groupID, role string, expiresAt *time.Time) (*Binding, error) {
	return s.createBinding(ctx, "create_group_binding", snap, nodeID, role, expiresAt, func(ctx context.Context, w *write, n *nodes.Node) (*Binding, error) {
		if err := s.checkGroupInTree(ctx, w, n, groupID); err != nil {
			return nil, err
		}
		b, err := s.insertSubjectBinding(ctx, w, "group_id", groupID, n.ID, role, expiresAt, "This group already has that role here.")
		if err != nil {
			return nil, err
		}
		w.invalidate(lifecycle.InvalidateRoot(n.RootID))
		return b, nil
	})
}

// CreateBinding binds role for a member already in the node's tree.
func (s *Store) CreateBinding(ctx context.Context, snap *feed.Snapshot, nodeID, memberID, role string, expiresAt *time.Time) (*Binding, error) {
	return s.createBinding(ctx, "create_binding", snap, nodeID, role, expiresAt, func(ctx context.Context, w *write, n *nodes.Node) (*Binding, error) {
		if err := s.checkInTree(ctx, w, n, memberID); err != nil {
			return nil, err
		}
		return s.insertBinding(ctx, w, memberID, n.ID, role, expiresAt)
	})
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

// ListBindings is the access list: every unexpired binding that counts at a
// node, on it and on each ancestor, or with inherited false only its own.
func (s *Store) ListBindings(ctx context.Context, nodeID string, inherited bool, limit int, startingAfter string) ([]*Binding, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_bindings", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if _, err := s.nodes.Get(ctx, nodeID); err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	if s.externalAuthz() {
		op.OK("ok")
		return []*Binding{}, false, nil
	}
	rows, err := s.pool.Query(ctx, bindingSelect("$5", `($2 OR b.node_id = $1) AND (b.expires_at IS NULL OR b.expires_at > $5) AND b.id > $3 ORDER BY b.id LIMIT $4`),
		nodeID, inherited, startingAfter, limit+1, s.now().UTC())
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := collectBindings(rows)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := dbx.Page(out, limit)
	op.OK("ok")
	return out, hasMore, nil
}

// PatchBinding changes a binding's role and/or expires_at. It returns the
// binding before and after.
func (s *Store) PatchBinding(ctx context.Context, snap *feed.Snapshot, nodeID, bindingID string, patch BindingPatch) (*Binding, *Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "patch_binding", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if snap.ExternalAuthz() {
		return nil, nil, op.Expected("disabled", errBuiltInRBACDisabled())
	}
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
	now := s.now().UTC()
	changed := before.Role != role || !timesEqual(before.ExpiresAt, expiresAt)
	if before.Role != role {
		// Replace any expired binding for the target (subject, node, role) so
		// patching onto a previously expired role succeeds, matching POST.
		var col, subject string
		if before.MemberID != nil {
			col, subject = "member_id", *before.MemberID
		} else if before.GroupID != nil {
			col, subject = "group_id", *before.GroupID
		}
		if col != "" {
			if _, err := w.tx.Exec(ctx, `
				DELETE FROM bindings
				WHERE `+col+` = $1 AND node_id = $2 AND role = $3 AND id <> $4
				  AND expires_at IS NOT NULL AND expires_at <= $5
			`, subject, n.ID, role, before.ID, now); err != nil {
				return nil, nil, op.Fail(err)
			}
		}
	}
	if _, err := w.tx.Exec(ctx, `
		UPDATE bindings SET role = $2, expires_at = $3, updated_at = CASE WHEN $4 THEN $5 ELSE updated_at END WHERE id = $1
	`, before.ID, role, expiresAt, changed, now); err != nil {
		if dbx.IsUniqueViolation(err) {
			msg := "This member already has that role here."
			if before.GroupID != nil {
				msg = "This group already has that role here."
			}
			return nil, nil, op.Expected("conflict", errors.ConflictError(msg, "role", role))
		}
		return nil, nil, op.Fail(err)
	}
	after, err := scanBinding(w.tx.QueryRow(ctx, bindingSelect("$3", `b.id = $2`), n.ID, before.ID, s.now().UTC()))
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	if changed {
		if before.MemberID != nil {
			w.invalidate(lifecycle.InvalidateMember(*before.MemberID))
		} else if before.GroupID != nil {
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
	b, err := scanBinding(w.tx.QueryRow(ctx, bindingSelect("$3", `b.id = $2 AND b.node_id = $1 FOR UPDATE OF b`), nodeID, bindingID, s.now().UTC()))
	if dbx.IsNoRows(err) {
		return nil, ErrNotFound
	}
	return b, err
}

// DeleteBinding removes a binding. A person's member left with no bindings is
// removed; a service account's isn't. A binding already gone is nil.
func (s *Store) DeleteBinding(ctx context.Context, snap *feed.Snapshot, nodeID, bindingID string) (*Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_binding", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if snap.ExternalAuthz() {
		return nil, op.Expected("disabled", errBuiltInRBACDisabled())
	}
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
	} else if b.GroupID != nil {
		w.invalidate(lifecycle.InvalidateRoot(n.RootID))
	}
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("deleted")
	return b, nil
}

// Membership is what one member holds at a node: its status and, in built-in
// RBAC mode, the unexpired bindings that count for it there and the permissions
// they grant. In external authorizer mode, ExternalAuthz is true and Bindings /
// Permissions are omitted.
type Membership struct {
	MemberID      string
	Status        string
	ExternalAuthz bool
	Bindings      []*Binding
	Permissions   []string
}

// Membership is GET /nodes/{node_id}/memberships/{member_id}. The member must
// be in the node's tree; a removed or suspended member, or a node with a
// deleted/purged chain, holds nothing.
func (s *Store) Membership(ctx context.Context, snap *feed.Snapshot, nodeID, memberID string) (*Membership, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_membership", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	n, err := s.nodes.GetTx(ctx, tx, nodeID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	m, err := s.getMember(ctx, tx, memberID, snap.ExternalAuthz())
	if err == nil && m.RootID != n.RootID {
		err = ErrNotFound
	}
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	if snap.ExternalAuthz() {
		op.OK("ok")
		return &Membership{MemberID: m.ID, Status: m.Status(), ExternalAuthz: true}, nil
	}

	now := s.now().UTC()
	rows, err := tx.Query(ctx, bindingSelect("$3", `(
		b.member_id = $2
		OR EXISTS (SELECT 1 FROM group_members gm WHERE gm.group_id = b.group_id AND gm.member_id = $2)
	) AND (b.expires_at IS NULL OR b.expires_at > $3) ORDER BY b.id`), n.ID, m.ID, now)
	if err != nil {
		return nil, op.Fail(err)
	}
	bindings, err := collectBindings(rows)
	if err != nil {
		return nil, op.Fail(err)
	}

	perms := []string{}
	if !m.Removed && !m.Suspended {
		var chainLive, principalActive bool
		err := tx.QueryRow(ctx, `
			SELECT
				NOT EXISTS (
					SELECT 1 FROM node_ancestors a JOIN nodes an ON an.id = a.ancestor_id
					WHERE a.node_id = $1 AND an.status <> 'active'
				),
				COALESCE((
					SELECT p.status = 'active' AND (sn.id IS NULL OR sn.status = 'active')
					FROM members m
					JOIN principals p ON p.id = m.principal_id
					LEFT JOIN nodes sn ON sn.id = p.node_id
					WHERE m.id = $2
				), false)
		`, n.ID, m.ID).Scan(&chainLive, &principalActive)
		if err != nil {
			return nil, op.Fail(err)
		}
		if chainLive && principalActive {
			held, heldCustom, err := access.HeldOnTargets(ctx, tx, snap, []access.MemberTarget{{MemberID: m.ID, NodeID: n.ID}}, false, now)
			if err != nil {
				return nil, op.Fail(err)
			}
			perms = access.ResolveWithCustom(snap, held[0], heldCustom[0]).List()
		}
	}
	op.OK("ok")
	return &Membership{MemberID: m.ID, Status: m.Status(), Bindings: bindings, Permissions: perms}, nil
}
