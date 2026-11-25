package members

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/access"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/nodes"
)

// CreateChild creates a node under parentID. The caller's member, if any,
// must be active in the parent's tree; in built-in RBAC mode it gets
// creator_role on the new node when the kind binds its creator.
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
		if err == ErrNotFound || (err == nil && (m.removed || m.suspended || m.rootID != n.RootID)) {
			return nil, nil, op.Expected("bad caller", ErrBadCaller)
		}
		if err != nil {
			return nil, nil, op.Fail(err)
		}
		if k, _ := snap.Kind(n.Kind); k.BindCreator && !snap.ExternalAuthz() {
			if b, err = s.insertBinding(ctx, w, m.id, n.ID, snap.CreatorRole, nil); err != nil {
				return nil, nil, op.Fail(err)
			}
		}
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	metrics.RecordNodeOps("created", 1)
	op.OK("created")
	return n, b, nil
}

// CreateRoot creates a root. The user, if any, must be an active person; when
// the kind binds its creator, they get a member in the new tree (and in
// built-in RBAC mode, creator_role at its root). It returns that member's id, or "".
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
	if k, _ := snap.Kind(n.Kind); u != nil && k.BindCreator {
		memberID, err = s.insertMember(ctx, w, u.principalID, PrincipalPerson, n.ID)
		if err == errPrincipalGone {
			// The account was deleted meanwhile: the header names no active user.
			return nil, "", op.Expected("bad caller", ErrBadCaller)
		}
		if err != nil {
			return nil, "", op.Fail(err)
		}
		if !snap.ExternalAuthz() {
			if _, err := s.insertBinding(ctx, w, memberID, n.ID, snap.CreatorRole, nil); err != nil {
				return nil, "", op.Fail(err)
			}
		}
		w.invalidate(lifecycle.InvalidateUser(u.ID))
	}
	if err := w.commit(ctx); err != nil {
		return nil, "", op.Fail(err)
	}
	metrics.RecordNodeOps("created", 1)
	op.OK("created")
	return n, memberID, nil
}

// --- lists (docs/concepts/access.md#lists) ---

// listed is a candidate for a filtered list, with the member whose bindings or
// external PDP policies decide it.
type listed struct {
	*nodes.Node
	memberID  string
	principal string
}

// ListChildren is the children of kind under parentID, live or, with
// deleted, deleted ones. A kind whose parent isn't the parent's kind is a 422,
// as on create. With a caller, only those the caller's member can see; the
// caller must be an active, unsuspended member of the parent's tree whose
// principal admit accepts. Without one, the full list.
func (s *Store) ListChildren(ctx context.Context, snap *feed.Snapshot, parentID, kind, callerID string, deleted bool, limit int, startingAfter string) ([]*nodes.Node, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_children", 4*dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	parent, err := s.nodes.Get(ctx, parentID)
	if err != nil {
		return nil, false, op.Reject(err, nodes.ErrNotFound)
	}
	if k, ok := snap.Kind(kind); !ok || k.Parent != parent.Kind {
		return nil, false, op.Expected("invalid", nodes.KindNotHere(kind))
	}
	var callerPrincipal string
	if callerID != "" {
		if id.Valid(callerID) {
			err := s.pool.QueryRow(ctx, `
				SELECT m.principal
				FROM members m
				JOIN principals p ON p.id = m.principal_id
				LEFT JOIN nodes sn ON sn.id = p.node_id
				WHERE m.id = $1 AND m.root_id = $2
				  AND m.removed_at IS NULL AND m.suspended_at IS NULL
				  AND p.status = 'active'
				  AND (sn.id IS NULL OR sn.status = 'active')
			`, callerID, parent.RootID).Scan(&callerPrincipal)
			if err != nil && !dbx.IsNoRows(err) {
				return nil, false, op.Fail(err)
			}
		}
		if callerPrincipal == "" {
			return nil, false, op.Expected("bad caller", ErrBadCaller)
		}
	}
	fetch := func(after string, n int) ([]*listed, error) {
		rows, err := s.pool.Query(ctx, `
			SELECT `+nodes.Columns("n")+`, $6::text, $7::text
			FROM nodes n
			WHERE n.parent_id = $1 AND n.kind = $2 AND n.status = $3 AND n.id > $4
			  AND NOT EXISTS (
			      SELECT 1 FROM node_ancestors a JOIN nodes an ON an.id = a.ancestor_id
			      WHERE a.node_id = n.id AND a.depth > 0 AND an.status <> 'active')
			ORDER BY n.id LIMIT $5
		`, parent.ID, kind, statusOf(deleted), after, n, callerID, callerPrincipal)
		if err != nil {
			return nil, err
		}
		return collectListed(rows)
	}
	out, hasMore, err := s.filterPage(ctx, snap, callerID != "", limit, startingAfter, fetch)
	if err != nil {
		return nil, false, op.Reject(err)
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
	if u.Status != StatusActive {
		op.OK("suspended")
		return []*nodes.Node{}, false, nil
	}
	fetch := func(after string, n int) ([]*listed, error) {
		rows, err := s.pool.Query(ctx, `
			SELECT `+nodes.Columns("n")+`, m.id, m.principal
			FROM nodes n
			JOIN members m ON m.root_id = n.root_id AND m.principal_id = $1 AND m.removed_at IS NULL AND m.suspended_at IS NULL
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
		return nil, false, op.Reject(err)
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

// collectListed scans node rows followed by the member id and principal kind.
func collectListed(rows pgx.Rows) ([]*listed, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*listed, error) {
		l := &listed{}
		n, err := nodes.ScanNode(r, &l.memberID, &l.principal)
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
	if filter && snap.ExternalAuthz() {
		// Cap at MaxPDPBatch (100) so limit=100 doesn't split into a 100+1 double PDP round-trip.
		batch = access.MaxPDPBatch
	}
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

// visible decides each candidate against its kind's list_permission, using
// either the deployment-wide external AuthZEN PDP or Plat5's built-in bindings.
func (s *Store) visible(ctx context.Context, snap *feed.Snapshot, cands []*listed) (map[string]bool, error) {
	out := make(map[string]bool, len(cands))
	if len(cands) == 0 {
		return out, nil
	}

	if snap.ExternalAuthz() {
		var extIDs []string
		var extCands []*listed
		var actions []string
		for _, c := range cands {
			k, ok := snap.Kind(c.Kind)
			if !ok {
				continue
			}
			extIDs = append(extIDs, c.ID)
			extCands = append(extCands, c)
			actions = append(actions, k.ListPermission)
		}
		if len(extIDs) == 0 {
			return out, nil
		}
		ancestors, err := s.nodes.Ancestors(ctx, extIDs)
		if err != nil {
			return nil, err
		}
		reqs := make([]access.AuthzenRequest, len(extCands))
		for i, c := range extCands {
			anc := ancestors[c.ID]
			chain := make([]access.ChainNode, 0, len(anc)+1)
			for _, a := range anc {
				chain = append(chain, access.ChainNode{Kind: a.Kind, ID: a.ID})
			}
			chain = append(chain, access.ChainNode{Kind: c.Kind, ID: c.ID})
			reqs[i] = access.BuildNodeAuthzenRequest(c.memberID, c.principal, c.RootID, actions[i], c.Kind, c.ID, chain)
		}
		decisions, err := access.EvaluateAuthorizerBatch(ctx, snap, s.pdp, reqs)
		if err != nil {
			return nil, err
		}
		for i, c := range extCands {
			out[c.ID] = decisions[i]
		}
		return out, nil
	}

	targets := make([]access.MemberTarget, len(cands))
	for i, c := range cands {
		targets[i] = access.MemberTarget{MemberID: c.memberID, NodeID: c.ID}
	}
	held, heldCustom, err := access.HeldOnTargets(ctx, s.pool, snap, targets, true, s.now().UTC())
	if err != nil {
		return nil, err
	}
	for i, c := range cands {
		k, ok := snap.Kind(c.Kind)
		out[c.ID] = ok && access.ResolveWithCustom(snap, held[i], heldCustom[i]).Has(k.ListPermission)
	}
	return out, nil
}
