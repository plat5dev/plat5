package members

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
)

// Evaluation is one access question: does the member hold the permission on
// the node, which the caller names as a node of kind
// (docs/reference/api/internal.md#access-evaluation). Ids are already read
// as lowercase ULIDs.
type Evaluation struct {
	MemberID, Kind, NodeID, Permission string
}

// Evaluate decides each evaluation, in order, with admit's resolution: the
// node's chain live, the member active in its tree with a principal admit
// accepts, and the member's bindings on the chain granting the permission.
// Anything else is false. One snapshot answers the whole batch, and each
// distinct node's chain is read once.
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
	roots, err := admissibleMembers(ctx, tx, memberIDs)
	if err != nil {
		return nil, op.Fail(err)
	}

	// Each (member, node) that passes the node and member steps is one pair
	// for the permission step.
	type key struct{ member, node string }
	pairOf := map[key]int{}
	var pairs []memberChain
	pairIdx := make([]int, len(evals))
	for i, e := range evals {
		pairIdx[i] = -1
		chain := chains[e.NodeID]
		if chain == nil || chain[len(chain)-1].Kind != e.Kind || roots[e.MemberID] != chain[0].ID {
			continue
		}
		k := key{e.MemberID, e.NodeID}
		p, ok := pairOf[k]
		if !ok {
			p = len(pairs)
			pairOf[k] = p
			pairs = append(pairs, memberChain{memberID: e.MemberID, nodes: chain})
		}
		pairIdx[i] = p
	}
	held, err := heldOnChains(ctx, tx, snap, pairs)
	if err != nil {
		return nil, op.Fail(err)
	}
	granted := make([]Permissions, len(pairs))
	for p := range pairs {
		granted[p] = Resolve(snap, held[p])
	}
	for i, e := range evals {
		out[i] = pairIdx[i] >= 0 && granted[pairIdx[i]].Has(e.Permission)
	}
	op.OK("ok")
	return out, nil
}

// liveChains reads each node's chain, root first, with the node last. A node
// that's unknown, or whose chain doesn't resolve as admit's would without
// allow_deleted (a deleted or purged node on it), has none.
func liveChains(ctx context.Context, q queryer, nodeIDs []string) (map[string][]ChainNode, error) {
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
		if !liveInChain(status, n.ID == nodeID, false) {
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

// admissibleMembers maps each active member whose principal admit would
// accept to its tree's root. Removed and unknown members are left out.
func admissibleMembers(ctx context.Context, q queryer, memberIDs []string) (map[string]string, error) {
	rows, err := q.Query(ctx, `
		SELECT m.id, m.root_id, p.status, sn.status
		FROM members m
		JOIN principals p ON p.id = m.principal_id
		LEFT JOIN nodes sn ON sn.id = p.node_id
		WHERE m.id = ANY($1) AND m.removed_at IS NULL
	`, distinct(memberIDs))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	var memberID, rootID, status string
	var saNodeStatus *string
	if _, err := pgx.ForEachRow(rows, []any{&memberID, &rootID, &status, &saNodeStatus}, func() error {
		if admissible(status, saNodeStatus) {
			out[memberID] = rootID
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
