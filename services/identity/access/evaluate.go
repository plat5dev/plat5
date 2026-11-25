package access

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/nodes"
)

// Evaluation is one access question: does the member hold the permission on
// the node, which the caller names as a node of kind
// (docs/reference/api/internal.md#access-evaluation). Ids are already read
// as lowercase ULIDs.
type Evaluation struct {
	MemberID, Kind, NodeID, Permission string
}

type memberInfo struct {
	rootID    string
	principal string
}

// Evaluate decides each evaluation, in order, with admit's resolution: the
// node's chain live, the member active in its tree with a principal admit
// accepts, and either built-in bindings (RBAC mode) or the external AuthZEN PDP
// (authorizer mode) granting the permission. Anything else is false.
func (s *Store) Evaluate(ctx context.Context, snap *feed.Snapshot, evals []Evaluation) ([]bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "evaluate", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	out := make([]bool, len(evals))
	if len(evals) == 0 {
		op.OK("ok")
		return out, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	var nodeIDs, memberIDs []string
	for _, e := range evals {
		nodeIDs = append(nodeIDs, e.NodeID)
		memberIDs = append(memberIDs, e.MemberID)
	}
	chains, err := liveChains(ctx, tx, nodeIDs)
	if err != nil {
		return nil, op.Fail(err)
	}
	members, err := admissibleMembers(ctx, tx, memberIDs)
	if err != nil {
		return nil, op.Fail(err)
	}

	if snap.ExternalAuthz() {
		_ = tx.Rollback(ctx)
		var extIdx []int
		var reqs []AuthzenRequest
		for i, e := range evals {
			chain := chains[e.NodeID]
			m, ok := members[e.MemberID]
			if !ok || chain == nil || chain[len(chain)-1].Kind != e.Kind || m.rootID != chain[0].ID {
				continue
			}
			extIdx = append(extIdx, i)
			reqs = append(reqs, BuildNodeAuthzenRequest(e.MemberID, m.principal, chain[0].ID, e.Permission, e.Kind, e.NodeID, chain))
		}
		if len(reqs) > 0 {
			decisions, err := EvaluateAuthorizerBatch(ctx, snap, s.pdp, reqs)
			if err != nil {
				return nil, op.Reject(err)
			}
			for j, idx := range extIdx {
				out[idx] = decisions[j]
			}
		}
		op.OK("ok")
		return out, nil
	}

	// Built-in RBAC: deduplicate (member, node) targets and resolve once.
	type key struct{ member, node string }
	targetOf := map[key]int{}
	var targets []MemberTarget
	targetIdx := make([]int, len(evals))
	for i, e := range evals {
		targetIdx[i] = -1
		chain := chains[e.NodeID]
		m, ok := members[e.MemberID]
		if !ok || chain == nil || chain[len(chain)-1].Kind != e.Kind || m.rootID != chain[0].ID {
			continue
		}
		k := key{e.MemberID, e.NodeID}
		p, exists := targetOf[k]
		if !exists {
			p = len(targets)
			targetOf[k] = p
			targets = append(targets, MemberTarget{MemberID: e.MemberID, NodeID: e.NodeID})
		}
		targetIdx[i] = p
	}
	held, heldCustom, err := HeldOnTargets(ctx, tx, snap, targets, false, s.now().UTC())
	if err != nil {
		return nil, op.Fail(err)
	}
	granted := make([]Permissions, len(targets))
	for p := range targets {
		granted[p] = ResolveWithCustom(snap, held[p], heldCustom[p])
	}
	for i, e := range evals {
		out[i] = targetIdx[i] >= 0 && granted[targetIdx[i]].Has(e.Permission)
	}
	op.OK("ok")
	return out, nil
}

// liveChains reads each node's chain, root first, with the node last. A node
// that's unknown, or has any non-active node on its chain, is omitted.
func liveChains(ctx context.Context, q dbx.Querier, nodeIDs []string) (map[string][]ChainNode, error) {
	rows, err := q.Query(ctx, `
		SELECT a.node_id, n.id, n.kind, n.status
		FROM node_ancestors a JOIN nodes n ON n.id = a.ancestor_id
		WHERE a.node_id = ANY($1)
		ORDER BY a.node_id, a.depth DESC
	`, distinct(nodeIDs))
	if err != nil {
		return nil, err
	}
	read := map[string][]ChainNode{}
	dead := map[string]bool{}
	var nodeID, status string
	var n ChainNode
	if _, err := pgx.ForEachRow(rows, []any{&nodeID, &n.ID, &n.Kind, &status}, func() error {
		if status != nodes.StatusActive {
			dead[nodeID] = true
		}
		read[nodeID] = append(read[nodeID], n)
		return nil
	}); err != nil {
		return nil, err
	}
	for id := range dead {
		delete(read, id)
	}
	return read, nil
}

// admissibleMembers maps each active unsuspended member whose principal admit
// would accept to its root_id and principal kind.
func admissibleMembers(ctx context.Context, q dbx.Querier, memberIDs []string) (map[string]memberInfo, error) {
	rows, err := q.Query(ctx, `
		SELECT m.id, m.root_id, m.principal, p.status, sn.status
		FROM members m
		JOIN principals p ON p.id = m.principal_id
		LEFT JOIN nodes sn ON sn.id = p.node_id
		WHERE m.id = ANY($1) AND m.removed_at IS NULL AND m.suspended_at IS NULL
	`, distinct(memberIDs))
	if err != nil {
		return nil, err
	}
	out := map[string]memberInfo{}
	var memberID, rootID, principal, status string
	var saNodeStatus *string
	if _, err := pgx.ForEachRow(rows, []any{&memberID, &rootID, &principal, &status, &saNodeStatus}, func() error {
		if admissible(status, saNodeStatus) {
			out[memberID] = memberInfo{rootID: rootID, principal: principal}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// distinct is ids without repeats, in first-seen order.
func distinct(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
