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
// unless an inherited binding with no expiry already holds it
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
		m, err := s.callerIn(ctx, w, callerID, n.RootID)
		if err != nil {
			return nil, nil, op.Reject(err, ErrNotMember)
		}
		held, err := s.holdsInherited(ctx, w, snap, m.id, n.ID, snap.CreatorRole)
		if err != nil {
			return nil, nil, op.Fail(err)
		}
		if !held {
			if b, err = s.insertBinding(ctx, w, m.id, n.ID, snap.CreatorRole, nil); err != nil {
				return nil, nil, op.Fail(err)
			}
		}
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("created")
	return n, b, nil
}

// callerIn is the caller's member, which must be active in rootID's tree.
func (s *Store) callerIn(ctx context.Context, w *write, callerID, rootID string) (*memberRow, error) {
	if !id.Valid(id.Member, callerID) {
		return nil, ErrNotMember
	}
	m, err := s.lockedMember(ctx, w, callerID)
	if err == ErrNotFound || (err == nil && (m.removed || m.rootID != rootID)) {
		return nil, ErrNotMember
	}
	return m, err
}

// holdsInherited is whether the member holds role, never expiring and
// counting, on a strict ancestor of nodeID.
func (s *Store) holdsInherited(ctx context.Context, w *write, snap *feed.Snapshot, memberID, nodeID, role string) (bool, error) {
	kinds, roles := bindablePairs(snap)
	var held bool
	err := w.tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM bindings b JOIN nodes bn ON bn.id = b.node_id
			WHERE b.member_id = $1 AND b.role = $3 AND b.expires_at IS NULL
			  AND b.node_id IN (SELECT ancestor_id FROM node_ancestors WHERE node_id = $2 AND depth > 0)
			  AND `+counts("$4", "$5", "$6")+`)
	`, memberID, nodeID, role, s.now().UTC(), kinds, roles).Scan(&held)
	return held, err
}

// CreateRoot creates a root. The caller, if any, is an active directory
// member; they get a member in the new tree with creator_role at its root.
func (s *Store) CreateRoot(ctx context.Context, snap *feed.Snapshot, in nodes.CreateInput, callerID string) (*nodes.Node, *Member, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_root_node", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	w, err := s.begin(ctx)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	var principalID string
	if callerID != "" {
		if err := nodes.LockTrees(ctx, w.tx, []string{s.signInID}); err != nil {
			return nil, nil, op.Fail(err)
		}
		m, err := s.callerIn(ctx, w, callerID, s.signInID)
		if err != nil {
			return nil, nil, op.Reject(err, ErrNotMember)
		}
		principalID = *m.principalID
	}
	in.ParentID = ""
	n, err := s.nodes.CreateTx(ctx, w.tx, snap, in)
	if err != nil {
		return nil, nil, op.Reject(err, nodes.ErrNotFound)
	}
	w.event(lifecycle.NodeEvent(lifecycle.TypeNodeCreated, n.Kind, n.ID, n.RootID))

	var m *Member
	if principalID != "" {
		memberID, err := s.insertMember(ctx, w, principalID, PrincipalPerson, n.ID)
		if err != nil {
			return nil, nil, op.Fail(err)
		}
		if _, err := s.insertBinding(ctx, w, memberID, n.ID, snap.CreatorRole, nil); err != nil {
			return nil, nil, op.Fail(err)
		}
		if m, err = s.getMember(ctx, w.tx, memberID); err != nil {
			return nil, nil, op.Fail(err)
		}
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("created")
	return n, m, nil
}

// --- filtered lists (docs/concepts/access.md#lists) ---

// ListedNode is a node in a filtered list, with its root for /members/{id}/nodes.
type ListedNode struct {
	*nodes.Node
	RootSlug  *string
	RootLabel string
	memberID  string // the member whose bindings decide
}

// ListChildren is the children of kind under parentID that the caller's
// member can read, or, with deleted, the deleted ones it could delete. The
// caller must be an active member of the parent's tree.
func (s *Store) ListChildren(ctx context.Context, snap *feed.Snapshot, parentID, kind, callerID string, deleted bool, limit int, startingAfter string) ([]*ListedNode, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_children", 4*dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	parent, err := s.nodes.Get(ctx, parentID)
	if err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	if !id.Valid(id.Member, callerID) {
		return nil, false, op.Expected("not a member", ErrNotMember)
	}
	var active bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM members WHERE id = $1 AND root_id = $2 AND removed_at IS NULL)
	`, callerID, parent.RootID).Scan(&active); err != nil {
		return nil, false, op.Fail(err)
	}
	if !active {
		return nil, false, op.Expected("not a member", ErrNotMember)
	}
	fetch := func(after string, n int) ([]*ListedNode, error) {
		rows, err := s.pool.Query(ctx, `
			SELECT `+nodes.Columns("n")+`, NULL::text, ''
			FROM nodes n
			WHERE n.parent_id = $1 AND n.kind = $2 AND n.status = $3 AND n.id > $4
			  AND NOT EXISTS (
			      SELECT 1 FROM node_ancestors a JOIN nodes an ON an.id = a.ancestor_id
			      WHERE a.node_id = n.id AND a.depth > 0 AND an.status <> 'active')
			ORDER BY n.id LIMIT $5
		`, parent.ID, kind, statusOf(deleted), after, n)
		if err != nil {
			return nil, err
		}
		return collectListed(rows, callerID)
	}
	out, hasMore, err := s.filterPage(ctx, snap, deleted, limit, startingAfter, fetch)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	op.OK("ok")
	return out, hasMore, nil
}

// ListPersonNodes is /members/{member_id}/nodes: nodes of kind (every kind
// when empty) across every tree the person is in, filtered the same way.
func (s *Store) ListPersonNodes(ctx context.Context, snap *feed.Snapshot, memberID, kind string, deleted bool, limit int, startingAfter string) ([]*ListedNode, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_person_nodes", 4*dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	principalID, err := s.personPrincipal(ctx, s.pool, memberID)
	if err != nil {
		return nil, false, op.Reject(err, ErrNotFound)
	}
	fetch := func(after string, n int) ([]*ListedNode, error) {
		rows, err := s.pool.Query(ctx, `
			SELECT `+nodes.Columns("n")+`, r.slug, r.label, m.id
			FROM nodes n
			JOIN members m ON m.root_id = n.root_id AND m.principal_id = $1 AND m.removed_at IS NULL
			JOIN nodes r ON r.id = n.root_id
			WHERE n.root_id <> $2 AND ($3 = '' OR n.kind = $3) AND n.status = $4 AND n.id > $5
			  AND NOT EXISTS (
			      SELECT 1 FROM node_ancestors a JOIN nodes an ON an.id = a.ancestor_id
			      WHERE a.node_id = n.id AND a.depth > 0 AND an.status <> 'active')
			ORDER BY n.id LIMIT $6
		`, principalID, s.signInID, kind, statusOf(deleted), after, n)
		if err != nil {
			return nil, err
		}
		return collectListed(rows, "")
	}
	out, hasMore, err := s.filterPage(ctx, snap, deleted, limit, startingAfter, fetch)
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

// collectListed scans node rows followed by root slug, root label and,
// unless memberID is given for all, the member whose bindings decide.
func collectListed(rows pgx.Rows, memberID string) ([]*ListedNode, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*ListedNode, error) {
		l := &ListedNode{memberID: memberID}
		extra := []any{&l.RootSlug, &l.RootLabel}
		if memberID == "" {
			extra = append(extra, &l.memberID)
		}
		n, err := nodes.ScanNode(r, extra...)
		l.Node = n
		return l, err
	})
}

// filterPage walks candidates in id order until it has a page of readable
// ones. A page can't stop short with nothing in it, or the caller would have
// no cursor, so it reads on until limit+1 are found or none are left.
func (s *Store) filterPage(ctx context.Context, snap *feed.Snapshot, deleted bool, limit int, after string, fetch func(after string, n int) ([]*ListedNode, error)) ([]*ListedNode, bool, error) {
	batch := max(limit+1, 100)
	var out []*ListedNode
	for {
		cands, err := fetch(after, batch)
		if err != nil {
			return nil, false, err
		}
		ok, err := s.readable(ctx, snap, cands, deleted)
		if err != nil {
			return nil, false, err
		}
		for _, c := range cands {
			if !ok[c.ID] {
				continue
			}
			out = append(out, c)
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

// readable decides each candidate from its member's bindings on it and its
// ancestors, against the kind's read route (or delete route, for deleted
// ones). A deleted node grants nothing except on itself in a deleted list;
// candidates come with live ancestors only.
func (s *Store) readable(ctx context.Context, snap *feed.Snapshot, cands []*ListedNode, deleted bool) (map[string]bool, error) {
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
		WHERE (bn.status = 'active' OR bn.id = c.node_id) AND `+counts("$3", "$4", "$5"),
		nodeIDs, memberIDs, s.now().UTC(), kinds, roles)
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
		need, ok := snap.ReadLabels(c.Kind)
		if deleted {
			need, ok = snap.DeleteLabels(c.Kind)
		}
		out[c.ID] = ok && Resolve(snap, held[c.ID]).Allows(need)
	}
	return out, nil
}

// --- roles ---

// RolesAt is GET /nodes/{node_id}/roles: the roles bindable at the node's
// kind, by slug, with default_role on a root that can be joined and
// creator_role where it's bindable.
type RolesAt struct {
	Roles       []feed.Role
	DefaultRole *string
	CreatorRole *string
}

func (s *Store) Roles(ctx context.Context, snap *feed.Snapshot, nodeID string) (*RolesAt, error) {
	n, err := s.nodes.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	out := &RolesAt{Roles: []feed.Role{}}
	for _, slug := range snap.RoleSlugs() {
		if r := snap.Roles[slug]; r.BindableAtKind(n.Kind) {
			out.Roles = append(out.Roles, r)
		}
	}
	if n.ParentID == nil && n.ID != s.signInID && snap.DefaultRole != "" {
		out.DefaultRole = &snap.DefaultRole
	}
	if r, ok := snap.Role(snap.CreatorRole); ok && r.BindableAtKind(n.Kind) {
		out.CreatorRole = &snap.CreatorRole
	}
	return out, nil
}
