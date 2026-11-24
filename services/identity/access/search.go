package access

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
)

const (
	DefaultSearchLimit = 100
	MaxSearchLimit     = 1000
)

// ResourceSearch is POST /access/v1/search/resource
// (docs/reference/api/internal.md#resource-search): which live nodes of Kind
// at or below ScopeNodeID (or the member's root when empty) MemberID holds
// Permission on.
type ResourceSearch struct {
	MemberID    string
	Kind        string
	Permission  string
	ScopeNodeID string
	Limit       int
	After       string
}

// ResourceSearchResult is SearchResources' answer. When All is true, the
// member holds the permission on every live node of the requested kind at or
// below the scope, and NodeIDs is empty.
type ResourceSearchResult struct {
	All       bool
	NodeIDs   []string
	NextToken string
}

// SearchResources answers which live nodes of in.Kind at or below in.ScopeNodeID
// (or the member's root when omitted) in.MemberID holds in.Permission on, using
// admit's exact resolution inside one repeatable-read snapshot.
func (s *Store) SearchResources(ctx context.Context, snap *feed.Snapshot, in ResourceSearch) (*ResourceSearchResult, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "search_resources", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	limit := in.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	roots, err := admissibleMembers(ctx, tx, []string{in.MemberID})
	if err != nil {
		return nil, op.Fail(err)
	}
	memberRoot, ok := roots[in.MemberID]
	if !ok {
		op.OK("no member")
		return &ResourceSearchResult{}, nil
	}

	scopeID := in.ScopeNodeID
	if scopeID == "" {
		scopeID = memberRoot
	}
	chains, err := liveChains(ctx, tx, []string{scopeID})
	if err != nil {
		return nil, op.Fail(err)
	}
	chain := chains[scopeID]
	if len(chain) == 0 || chain[0].ID != memberRoot {
		op.OK("no scope")
		return &ResourceSearchResult{}, nil
	}

	scopeKind := chain[len(chain)-1].Kind
	if !kindAtOrBelow(snap, in.Kind, scopeKind) {
		op.OK("unreachable kind")
		return &ResourceSearchResult{}, nil
	}

	kinds, roles := grantingPairs(snap, in.Permission)
	if len(kinds) == 0 {
		op.OK("no roles")
		return &ResourceSearchResult{}, nil
	}

	// Short-circuit: a binding on the scope or an ancestor granting the
	// permission covers every live node of in.Kind at or below scopeID.
	held, err := heldOnChains(ctx, tx, snap, []memberChain{{memberID: in.MemberID, nodes: chain}})
	if err != nil {
		return nil, op.Fail(err)
	}
	if Resolve(snap, held[0]).Has(in.Permission) {
		op.OK("all")
		return &ResourceSearchResult{All: true}, nil
	}

	// Sparse case: anchor on the member's granting bindings (direct and via
	// groups) at or below scopeID, then expand downward to matching live nodes.
	// This keeps work proportional to the member's bindings rather than the
	// total number of nodes under scopeID.
	rows, err := tx.Query(ctx, `
		WITH granting_nodes AS (
			SELECT DISTINCT b.node_id
			FROM (
				SELECT node_id, role FROM bindings WHERE member_id = $4
				UNION ALL
				SELECT b.node_id, b.role
				FROM group_members gm
				JOIN bindings b ON b.group_id = gm.group_id
				WHERE gm.member_id = $4
			) b
			JOIN nodes bn ON bn.id = b.node_id
			JOIN node_ancestors in_scope ON in_scope.node_id = bn.id AND in_scope.ancestor_id = $1
			WHERE bn.status = 'active'
			  AND `+CountsSQL("$5", "$6")+`
		)
		SELECT DISTINCT n.id
		FROM granting_nodes g
		JOIN node_ancestors ba ON ba.ancestor_id = g.node_id
		JOIN nodes n ON n.id = ba.node_id
		WHERE n.kind = $2
		  AND n.status = 'active'
		  AND n.id > $3
		  AND NOT EXISTS (
		      SELECT 1 FROM node_ancestors da JOIN nodes dn ON dn.id = da.ancestor_id
		      WHERE da.node_id = n.id AND dn.status <> 'active'
		  )
		ORDER BY n.id ASC
		LIMIT $7
	`, scopeID, in.Kind, in.After, in.MemberID, kinds, roles, limit+1)
	if err != nil {
		return nil, op.Fail(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, op.Fail(err)
	}
	ids, hasMore := page(ids, limit)
	res := &ResourceSearchResult{NodeIDs: ids}
	if hasMore && len(ids) > 0 {
		res.NextToken = ids[len(ids)-1]
	}
	op.OK("ok")
	return res, nil
}

// kindAtOrBelow is whether kind exists in snap and is scopeKind or a
// descendant kind of scopeKind.
func kindAtOrBelow(snap *feed.Snapshot, kind, scopeKind string) bool {
	k, ok := snap.Kind(kind)
	if !ok {
		return false
	}
	for {
		if k.Name == scopeKind {
			return true
		}
		if k.Parent == "" {
			return false
		}
		k, ok = snap.Kind(k.Parent)
		if !ok {
			return false
		}
	}
}

// grantingPairs is every (kind, role) pair where role is bindable at kind and
// grants permission (through its permissions, includes, or "*").
func grantingPairs(snap *feed.Snapshot, permission string) (kinds, roles []string) {
	for _, slug := range snap.RoleSlugs() {
		if !Resolve(snap, []string{slug}).Has(permission) {
			continue
		}
		for _, k := range snap.Roles[slug].BindableAt {
			kinds = append(kinds, k)
			roles = append(roles, slug)
		}
	}
	return kinds, roles
}
