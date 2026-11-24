package members

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/nodes"
)

// Group is a flat collection of active members owned by one root tree
// (docs/concepts/model.md, docs/reference/api/identity.md#groups).
type Group struct {
	ID        string
	Name      string
	RootID    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const groupSelect = `SELECT id, name, root_id, created_at, updated_at FROM groups WHERE `

func scanGroup(row dbx.Scannable) (*Group, error) {
	var g Group
	if err := row.Scan(&g.ID, &g.Name, &g.RootID, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, err
	}
	return &g, nil
}

func (s *Store) getGroup(ctx context.Context, q dbx.Querier, rootID, groupID string) (*Group, error) {
	if !id.Valid(groupID) {
		return nil, ErrNotFound
	}
	g, err := scanGroup(q.QueryRow(ctx, groupSelect+`id = $1 AND root_id = $2`, groupID, rootID))
	if dbx.IsNoRows(err) {
		return nil, ErrNotFound
	}
	return g, err
}

func (s *Store) lockedGroup(ctx context.Context, w *write, rootID, groupID string) (*Group, error) {
	if !id.Valid(groupID) {
		return nil, ErrNotFound
	}
	g, err := scanGroup(w.tx.QueryRow(ctx, groupSelect+`id = $1 AND root_id = $2 FOR UPDATE`, groupID, rootID))
	if dbx.IsNoRows(err) {
		return nil, ErrNotFound
	}
	return g, err
}

// CreateGroup creates a group belonging to rootID.
func (s *Store) CreateGroup(ctx context.Context, rootID, name string) (*Group, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_group", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	name, err := normalizeName(name)
	if err != nil {
		return nil, op.Expected("invalid", err)
	}
	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	root, err := s.lockedRoot(ctx, w, rootID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if err := nodes.Writable(root); err != nil {
		return nil, op.Reject(err)
	}
	now := s.now().UTC()
	g := &Group{ID: id.New(), Name: name, RootID: root.ID, CreatedAt: now, UpdatedAt: now}
	if _, err := w.tx.Exec(ctx, `
		INSERT INTO groups (id, root_id, name, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)
	`, g.ID, g.RootID, g.Name, now); err != nil {
		return nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("created")
	return g, nil
}

// ListGroups is a root's groups, id ascending.
func (s *Store) ListGroups(ctx context.Context, rootID string, limit int, startingAfter string) ([]*Group, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_groups", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if err := s.checkRoot(ctx, rootID); err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	rows, err := s.pool.Query(ctx, groupSelect+`root_id = $1 AND id > $2 ORDER BY id LIMIT $3`,
		rootID, startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Group, error) { return scanGroup(r) })
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := page(out, limit)
	op.OK("ok")
	return out, hasMore, nil
}

// GetGroup is a group of rootID.
func (s *Store) GetGroup(ctx context.Context, rootID, groupID string) (*Group, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_group", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if err := s.checkRoot(ctx, rootID); err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	g, err := s.getGroup(ctx, s.pool, rootID, groupID)
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	op.OK("ok")
	return g, nil
}

// RenameGroup sets a group's name. It returns the name before.
func (s *Store) RenameGroup(ctx context.Context, rootID, groupID, name string) (string, *Group, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "rename_group", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	name, err := normalizeName(name)
	if err != nil {
		return "", nil, op.Expected("invalid", err)
	}
	w, err := s.begin(ctx)
	if err != nil {
		return "", nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	root, err := s.lockedRoot(ctx, w, rootID)
	if err != nil {
		return "", nil, op.Reject(err, nodes.ErrNotFound)
	}
	if err := nodes.Writable(root); err != nil {
		return "", nil, op.Reject(err)
	}
	before, err := s.lockedGroup(ctx, w, root.ID, groupID)
	if err != nil {
		return "", nil, op.Reject(err, ErrNotFound)
	}
	after, err := scanGroup(w.tx.QueryRow(ctx, `
		UPDATE groups SET name = $2, updated_at = CASE WHEN name = $2 THEN updated_at ELSE $3 END
		WHERE id = $1 RETURNING id, name, root_id, created_at, updated_at
	`, before.ID, name, s.now().UTC()))
	if err != nil {
		return "", nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return "", nil, op.Fail(err)
	}
	op.OK("ok")
	return before.Name, after, nil
}

// DeleteGroup deletes a group, its memberships and its bindings. A person's
// member left with no direct bindings and no group memberships is removed.
// Deleting a group already gone is nil.
func (s *Store) DeleteGroup(ctx context.Context, rootID, groupID string) (*Group, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_group", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	root, err := s.lockedRoot(ctx, w, rootID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	g, err := s.lockedGroup(ctx, w, root.ID, groupID)
	if err == ErrNotFound {
		op.OK("unchanged")
		return nil, nil
	}
	if err != nil {
		return nil, op.Fail(err)
	}
	rows, err := w.tx.Query(ctx, `SELECT member_id FROM group_members WHERE group_id = $1`, g.ID)
	if err != nil {
		return nil, op.Fail(err)
	}
	formerMembers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, op.Fail(err)
	}
	if _, err := w.tx.Exec(ctx, `DELETE FROM groups WHERE id = $1`, g.ID); err != nil {
		return nil, op.Fail(err)
	}
	w.invalidate(lifecycle.InvalidateRoot(root.ID))
	if err := s.removeEmptyPeople(ctx, w, formerMembers); err != nil {
		return nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("deleted")
	return g, nil
}

// ListGroupMembers lists active members in a group, member id ascending.
func (s *Store) ListGroupMembers(ctx context.Context, rootID, groupID string, limit int, startingAfter string) ([]*Member, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_group_members", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if err := s.checkRoot(ctx, rootID); err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	if _, err := s.getGroup(ctx, s.pool, rootID, groupID); err != nil {
		return nil, false, op.Reject(err, ErrNotFound)
	}
	rows, err := s.pool.Query(ctx, memberSelect(`
		m.removed_at IS NULL
		AND EXISTS (SELECT 1 FROM group_members gm WHERE gm.group_id = $1 AND gm.member_id = m.id)
		AND m.id > $2
		ORDER BY m.id LIMIT $3
	`), groupID, startingAfter, limit+1)
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

// AddGroupMember adds an active member of rootID to a group of rootID.
func (s *Store) AddGroupMember(ctx context.Context, rootID, groupID, memberID string) (*Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "add_group_member", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	root, err := s.lockedRoot(ctx, w, rootID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if err := nodes.Writable(root); err != nil {
		return nil, op.Reject(err)
	}
	g, err := s.lockedGroup(ctx, w, root.ID, groupID)
	if err != nil {
		return nil, op.Reject(err, ErrNotFound)
	}
	if err := s.checkInTree(ctx, w, root, memberID); err != nil {
		return nil, op.Reject(err)
	}
	if _, err := w.tx.Exec(ctx, `
		INSERT INTO group_members (group_id, member_id, created_at) VALUES ($1, $2, $3)
	`, g.ID, memberID, s.now().UTC()); err != nil {
		if dbx.IsUniqueViolation(err) {
			return nil, op.Expected("exists", errors.ConflictError("This member is already in this group.", "member_id", memberID))
		}
		return nil, op.Fail(err)
	}
	m, err := s.getMember(ctx, w.tx, memberID)
	if err != nil {
		return nil, op.Fail(err)
	}
	w.invalidate(lifecycle.InvalidateMember(memberID))
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("added")
	return m, nil
}

// RemoveGroupMember removes memberID from groupID. A person's member left with
// no direct bindings and no group memberships is removed. Removing an absent
// membership or from a group already gone is unchanged.
func (s *Store) RemoveGroupMember(ctx context.Context, rootID, groupID, memberID string) (bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "remove_group_member", dbx.DefaultTimeout)
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
	g, err := s.lockedGroup(ctx, w, root.ID, groupID)
	if err == ErrNotFound {
		op.OK("unchanged")
		return false, nil
	}
	if err != nil {
		return false, op.Fail(err)
	}
	if !id.Valid(memberID) {
		op.OK("unchanged")
		return false, nil
	}
	tag, err := w.tx.Exec(ctx, `DELETE FROM group_members WHERE group_id = $1 AND member_id = $2`, g.ID, memberID)
	if err != nil {
		return false, op.Fail(err)
	}
	if tag.RowsAffected() == 0 {
		op.OK("unchanged")
		return false, nil
	}
	w.invalidate(lifecycle.InvalidateMember(memberID))
	if err := s.removeEmptyPeople(ctx, w, []string{memberID}); err != nil {
		return false, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return false, op.Fail(err)
	}
	op.OK("removed")
	return true, nil
}

// checkGroupInTree refuses a binding for a group that doesn't belong to n's tree.
func (s *Store) checkGroupInTree(ctx context.Context, w *write, n *nodes.Node, groupID string) error {
	_, err := s.lockedGroup(ctx, w, n.RootID, groupID)
	if err == ErrNotFound {
		var rootKind string
		if err := w.tx.QueryRow(ctx, `SELECT kind FROM nodes WHERE id = $1`, n.RootID).Scan(&rootKind); err != nil {
			return err
		}
		return errors.FieldError("group_id", fmt.Sprintf("That group doesn't belong to this %s.", rootKind))
	}
	return err
}
