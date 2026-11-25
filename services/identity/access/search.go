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
// admit's exact resolution inside one repeatable-read snapshot (releasing the
// transaction before calling the external AuthZEN PDP in authorizer mode).
func (s *Store) SearchResources(ctx context.Context, snap *feed.Snapshot, in ResourceSearch) (*ResourceSearchResult, error) {
	timeout := dbx.DefaultTimeout
	if snap.ExternalAuthz() {
		timeout = 4 * dbx.DefaultTimeout
	}
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "search_resources", timeout)
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

	members, err := admissibleMembers(ctx, tx, []string{in.MemberID})
	if err != nil {
		return nil, op.Fail(err)
	}
	member, ok := members[in.MemberID]
	if !ok {
		op.OK("no member")
		return &ResourceSearchResult{}, nil
	}

	scopeID := in.ScopeNodeID
	if scopeID == "" {
		scopeID = member.rootID
	}
	chains, err := liveChains(ctx, tx, []string{scopeID})
	if err != nil {
		return nil, op.Fail(err)
	}
	chain := chains[scopeID]
	if len(chain) == 0 || chain[0].ID != member.rootID {
		op.OK("no scope")
		return &ResourceSearchResult{}, nil
	}

	scopeKind := chain[len(chain)-1].Kind
	if !kindAtOrBelow(snap, in.Kind, scopeKind) {
		op.OK("unreachable kind")
		return &ResourceSearchResult{}, nil
	}

	if snap.ExternalAuthz() {
		// Release the repeatable-read transaction before making outbound PDP calls.
		_ = tx.Rollback(ctx)
		res, err := s.searchWithAuthorizer(ctx, snap, in, scopeID, member, limit)
		if err != nil {
			return nil, op.Reject(err)
		}
		op.OK("ok")
		return res, nil
	}

	now := s.now().UTC()
	// Short-circuit: a binding on the scope or an ancestor granting the
	// permission covers every live node of in.Kind at or below scopeID.
	held, heldCustom, err := HeldOnTargets(ctx, tx, snap, []MemberTarget{{MemberID: in.MemberID, NodeID: scopeID}}, false, now)
	if err != nil {
		return nil, op.Fail(err)
	}
	if ResolveWithCustom(snap, held[0], heldCustom[0]).Has(in.Permission) {
		op.OK("all")
		return &ResourceSearchResult{All: true}, nil
	}

	kinds, roles := snap.GrantingPairs(in.Permission)

	// Sparse case: anchor on the member's granting bindings (direct and via
	// groups, static or custom roles) at or below scopeID, then expand downward
	// to matching live nodes.
	rows, err := tx.Query(ctx, `
		WITH granting_nodes AS (
			SELECT DISTINCT b.node_id
			FROM (
				SELECT node_id, role FROM bindings WHERE member_id = $4 AND (expires_at IS NULL OR expires_at > $8)
				UNION ALL
				SELECT b.node_id, b.role
				FROM group_members gm
				JOIN bindings b ON b.group_id = gm.group_id
				WHERE gm.member_id = $4 AND (b.expires_at IS NULL OR b.expires_at > $8)
			) b
			JOIN nodes bn ON bn.id = b.node_id
			JOIN node_ancestors in_scope ON in_scope.node_id = bn.id AND in_scope.ancestor_id = $1
			WHERE bn.status = 'active'
			  AND (
				(bn.kind, b.role) IN (SELECT * FROM unnest($5::text[], $6::text[]))
				OR ($10 AND EXISTS (
					SELECT 1 FROM org_roles cr
					WHERE cr.id = b.role AND cr.root_id = bn.root_id
					  AND bn.kind = ANY(cr.bindable_at)
					  AND $9 <> '*' AND $9 = ANY(cr.permissions)
				))
			  )
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
	`, scopeID, in.Kind, in.After, in.MemberID, kinds, roles, limit+1, now, in.Permission, snap.CustomRoles)
	if err != nil {
		return nil, op.Fail(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, op.Fail(err)
	}
	ids, hasMore := dbx.Page(ids, limit)
	res := &ResourceSearchResult{NodeIDs: ids}
	if hasMore && len(ids) > 0 {
		res.NextToken = ids[len(ids)-1]
	}
	op.OK("ok")
	return res, nil
}

// searchWithAuthorizer walks live candidate nodes of in.Kind under scopeID in
// id order, evaluating them in batches of MaxPDPBatch against the external
// AuthZEN PDP. No Postgres transaction is held open across HTTP calls.
func (s *Store) searchWithAuthorizer(
	ctx context.Context,
	snap *feed.Snapshot,
	in ResourceSearch,
	scopeID string,
	member memberInfo,
	limit int,
) (*ResourceSearchResult, error) {
	after := in.After
	var out []string
	for {
		rows, err := s.pool.Query(ctx, `
			SELECT n.id
			FROM node_ancestors a
			JOIN nodes n ON n.id = a.node_id
			WHERE a.ancestor_id = $1
			  AND n.kind = $2
			  AND n.status = 'active'
			  AND n.id > $3
			  AND NOT EXISTS (
			      SELECT 1 FROM node_ancestors da JOIN nodes dn ON dn.id = da.ancestor_id
			      WHERE da.node_id = n.id AND dn.status <> 'active'
			  )
			ORDER BY n.id ASC
			LIMIT $4
		`, scopeID, in.Kind, after, MaxPDPBatch)
		if err != nil {
			return nil, err
		}
		cands, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		if len(cands) == 0 {
			return &ResourceSearchResult{NodeIDs: out}, nil
		}

		chains, err := liveChains(ctx, s.pool, cands)
		if err != nil {
			return nil, err
		}
		// Skip candidates deleted between the candidate query and liveChains.
		liveIDs := make([]string, 0, len(cands))
		reqs := make([]AuthzenRequest, 0, len(cands))
		for _, cid := range cands {
			chain := chains[cid]
			if len(chain) == 0 {
				continue
			}
			liveIDs = append(liveIDs, cid)
			reqs = append(reqs, BuildNodeAuthzenRequest(in.MemberID, member.principal, member.rootID, in.Permission, in.Kind, cid, chain))
		}
		if len(reqs) > 0 {
			decisions, err := EvaluateAuthorizerBatch(ctx, snap, s.pdp, reqs)
			if err != nil {
				return nil, err
			}
			for i, cid := range liveIDs {
				if !decisions[i] {
					continue
				}
				out = append(out, cid)
				if len(out) > limit {
					trimmed := out[:limit]
					return &ResourceSearchResult{
						NodeIDs:   trimmed,
						NextToken: trimmed[len(trimmed)-1],
					}, nil
				}
			}
		}
		if len(cands) < MaxPDPBatch {
			return &ResourceSearchResult{NodeIDs: out}, nil
		}
		after = cands[len(cands)-1]
	}
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
