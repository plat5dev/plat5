package members

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/nodes"
)

// CreateChild creates a node under parentID. The caller's member, if any,
// must be active in the parent's tree; it gets creator_role on the new node
// (docs/concepts/access.md#assigning).
func (s *Store) CreateChild(ctx context.Context, snap *feed.Snapshot, parentID string, in nodes.CreateInput, callerID string) (*nodes.Node, *Binding, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_child_node", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	in.ParentID = parentID
	n, err := s.nodes.CreateTx(ctx, w.tx, snap, in)
	if err != nil {
		return nil, nil, op.Reject(err, nodes.ErrNotFound)
	}
	w.event(lifecycle.NodeEvent(lifecycle.TypeNodeCreated, n.Kind, n.ID, n.RootID))

	var b *Binding
	if callerID != "" {
		m, err := s.lockedMember(ctx, w, callerID)
		if err == ErrNotFound || (err == nil && (m.removed || m.rootID != n.RootID)) {
			return nil, nil, op.Expected("bad caller", ErrBadCaller)
		}
		if err != nil {
			return nil, nil, op.Fail(err)
		}
		if b, err = s.insertBinding(ctx, w, m.id, n.ID, snap.CreatorRole); err != nil {
			return nil, nil, op.Fail(err)
		}
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("created")
	return n, b, nil
}

// CreateRoot creates a root. The user, if any, must be an active person; they
// get a member in the new tree with creator_role at its root. It returns that
// member's id, or "".
func (s *Store) CreateRoot(ctx context.Context, snap *feed.Snapshot, in nodes.CreateInput, userID string) (*nodes.Node, string, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_root_node", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return nil, "", op.Fail(err)
	}
	defer w.rollback(ctx)
	var u *User
	if userID != "" {
		u, err = s.user(ctx, w.tx, userID)
		if err == ErrNotFound || (err == nil && u.Status != StatusActive) {
			return nil, "", op.Expected("bad caller", ErrBadCaller)
		}
		if err != nil {
			return nil, "", op.Fail(err)
		}
	}
	in.ParentID = ""
	n, err := s.nodes.CreateTx(ctx, w.tx, snap, in)
	if err != nil {
		return nil, "", op.Reject(err, nodes.ErrNotFound)
	}
	w.event(lifecycle.NodeEvent(lifecycle.TypeNodeCreated, n.Kind, n.ID, n.RootID))

	var memberID string
	if u != nil {
		if memberID, err = s.insertMember(ctx, w, u.principalID, PrincipalPerson, n.ID); err != nil {
			return nil, "", op.Fail(err)
		}
		if _, err := s.insertBinding(ctx, w, memberID, n.ID, snap.CreatorRole); err != nil {
			return nil, "", op.Fail(err)
		}
	}
	if err := w.commit(ctx); err != nil {
		return nil, "", op.Fail(err)
	}
	op.OK("created")
	return n, memberID, nil
}

// --- lists (docs/concepts/access.md#lists) ---

// listed is a candidate for a filtered list, with the member whose bindings
// decide it.
type listed struct {
	*nodes.Node
	memberID string
}

// ListChildren is the children of kind under parentID, live or, with
// deleted, deleted ones. With a caller, only those the caller's member can
// see; the caller must be an active member of the parent's tree. Without one,
// the full list.
func (s *Store) ListChildren(ctx context.Context, snap *feed.Snapshot, parentID, kind, callerID string, deleted bool, limit int, startingAfter string) ([]*nodes.Node, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_children", 4*dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	parent, err := s.nodes.Get(ctx, parentID)
	if err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	if callerID != "" {
		var active bool
		if id.Valid(callerID) {
			if err := s.pool.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM members WHERE id = $1 AND root_id = $2 AND removed_at IS NULL)
			`, callerID, parent.RootID).Scan(&active); err != nil {
				return nil, false, op.Fail(err)
			}
		}
		if !active {
			return nil, false, op.Expected("bad caller", ErrBadCaller)
		}
	}
	fetch := func(after string, n int) ([]*listed, error) {
		rows, err := s.pool.Query(ctx, `
			SELECT `+nodes.Columns("n")+`, $6::text
			FROM nodes n
			WHERE n.parent_id = $1 AND n.kind = $2 AND n.status = $3 AND n.id > $4
			  AND NOT EXISTS (
			      SELECT 1 FROM node_ancestors a JOIN nodes an ON an.id = a.ancestor_id
			      WHERE a.node_id = n.id AND a.depth > 0 AND an.status <> 'active')
			ORDER BY n.id LIMIT $5
		`, parent.ID, kind, statusOf(deleted), after, n, callerID)
		if err != nil {
			return nil, err
		}
		return collectListed(rows)
	}
	out, hasMore, err := s.filterPage(ctx, snap, callerID != "", limit, startingAfter, fetch)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	op.OK("ok")
	return out, hasMore, nil
}

// ListUserNodes is /users/{user_id}/nodes: nodes of kind (every kind when
// empty) across every tree the person is in, filtered to what they can see.
func (s *Store) ListUserNodes(ctx context.Context, snap *feed.Snapshot, userID, kind string, deleted bool, limit int, startingAfter string) ([]*nodes.Node, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_user_nodes", 4*dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	u, err := s.user(ctx, s.pool, userID)
	if err != nil {
		return nil, false, op.Reject(err, ErrNotFound)
	}
	fetch := func(after string, n int) ([]*listed, error) {
		rows, err := s.pool.Query(ctx, `
			SELECT `+nodes.Columns("n")+`, m.id
			FROM nodes n
			JOIN members m ON m.root_id = n.root_id AND m.principal_id = $1 AND m.removed_at IS NULL
			WHERE ($2 = '' OR n.kind = $2) AND n.status = $3 AND n.id > $4
			  AND NOT EXISTS (
			      SELECT 1 FROM node_ancestors a JOIN nodes an ON an.id = a.ancestor_id
			      WHERE a.node_id = n.id AND a.depth > 0 AND an.status <> 'active')
			ORDER BY n.id LIMIT $5
		`, u.principalID, kind, statusOf(deleted), after, n)
		if err != nil {
			return nil, err
		}
		return collectListed(rows)
	}
	out, hasMore, err := s.filterPage(ctx, snap, true, limit, startingAfter, fetch)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	op.OK("ok")
	return out, hasMore, nil
}

func statusOf(deleted bool) string {
	if deleted {
		return nodes.StatusDeleted
	}
	return nodes.StatusActive
}

// collectListed scans node rows followed by the member whose bindings decide.
func collectListed(rows pgx.Rows) ([]*listed, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*listed, error) {
		l := &listed{}
		n, err := nodes.ScanNode(r, &l.memberID)
		l.Node = n
		return l, err
	})
}

// filterPage walks candidates in id order until it has a page, of visible
// ones when filter is set. A page can't stop short with nothing in it, or the
// caller would have no cursor, so it reads on until limit+1 are found or none
// are left.
func (s *Store) filterPage(ctx context.Context, snap *feed.Snapshot, filter bool, limit int, after string, fetch func(after string, n int) ([]*listed, error)) ([]*nodes.Node, bool, error) {
	batch := max(limit+1, 100)
	var out []*nodes.Node
	for {
		cands, err := fetch(after, batch)
		if err != nil {
			return nil, false, err
		}
		ok := map[string]bool{}
		if filter {
			if ok, err = s.visible(ctx, snap, cands); err != nil {
				return nil, false, err
			}
		}
		for _, c := range cands {
			if filter && !ok[c.ID] {
				continue
			}
			out = append(out, c.Node)
			if len(out) > limit {
				return out[:limit], true, nil
			}
		}
		if len(cands) < batch {
			return out, false, nil
		}
		after = cands[len(cands)-1].ID
	}
}

// visible decides each candidate from its member's bindings on it and its
// ancestors, against its kind's list_permission. A deleted node grants
// nothing except on itself; candidates come with live ancestors only.
func (s *Store) visible(ctx context.Context, snap *feed.Snapshot, cands []*listed) (map[string]bool, error) {
	out := make(map[string]bool, len(cands))
	if len(cands) == 0 {
		return out, nil
	}
	nodeIDs := make([]string, len(cands))
	memberIDs := make([]string, len(cands))
	for i, c := range cands {
		nodeIDs[i], memberIDs[i] = c.ID, c.memberID
	}
	kinds, roles := bindablePairs(snap)
	rows, err := s.pool.Query(ctx, `
		SELECT c.node_id, b.role
		FROM unnest($1::text[], $2::text[]) AS c (node_id, member_id)
		JOIN node_ancestors a ON a.node_id = c.node_id
		JOIN bindings b ON b.node_id = a.ancestor_id AND b.member_id = c.member_id
		JOIN nodes bn ON bn.id = b.node_id
		WHERE (bn.status = 'active' OR bn.id = c.node_id) AND `+counts("$3", "$4"),
		nodeIDs, memberIDs, kinds, roles)
	if err != nil {
		return nil, err
	}
	held := map[string][]string{}
	var nodeID, role string
	if _, err := pgx.ForEachRow(rows, []any{&nodeID, &role}, func() error {
		held[nodeID] = append(held[nodeID], role)
		return nil
	}); err != nil {
		return nil, err
	}
	for _, c := range cands {
		k, ok := snap.Kind(c.Kind)
		out[c.ID] = ok && Resolve(snap, held[c.ID]).Has(k.ListPermission)
	}
	return out, nil
}

// --- roles ---

// Roles is GET /nodes/{node_id}/roles: the roles bindable at the node's kind,
// by slug.
func (s *Store) Roles(ctx context.Context, snap *feed.Snapshot, nodeID string) ([]feed.Role, error) {
	n, err := s.nodes.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	out := []feed.Role{}
	for _, slug := range snap.RoleSlugs() {
		if r := snap.Roles[slug]; r.BindableAtKind(n.Kind) {
			out = append(out, r)
		}
	}
	return out, nil
}
