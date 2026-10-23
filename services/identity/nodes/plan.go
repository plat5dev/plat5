package nodes

import (
	"context"
	"slices"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

// planSample is how many ids a narrowed parent's count shows.
const planSample = 5

// PlanRequest is POST /internal/plan: the kinds a plan removes and the parent
// kinds it drops from a kind (docs/reference/api/internal.md#plan-counts).
type PlanRequest struct {
	KindsRemoved    []string   `json:"kinds_removed"`
	ParentsNarrowed []Narrowed `json:"parents_narrowed"`
}

type Narrowed struct {
	Kind    string   `json:"kind"`
	Removed []string `json:"removed"`
}

// Effects is what removing a kind would lose: its nodes and every descendant,
// live and deleted, the bindings on them and of service accounts born at
// them, those service accounts, and their keys. Expired bindings and revoked
// keys never work again, so they don't count.
type Effects struct {
	Nodes           int64 `json:"nodes"`
	Bindings        int64 `json:"bindings"`
	ServiceAccounts int64 `json:"service_accounts"`
	Keys            int64 `json:"keys"`
}

// InTheWay is the kind's nodes under a parent kind the config drops, with a
// few ids.
type InTheWay struct {
	Nodes  int64    `json:"nodes"`
	Sample []string `json:"sample"`
}

type PlanCounts struct {
	KindsRemoved    map[string]Effects  `json:"kinds_removed"`
	ParentsNarrowed map[string]InTheWay `json:"parents_narrowed"`
}

// PlanCounts counts, in one snapshot, what a plan's kind removals and
// narrowed parents touch. Purged nodes never count. A narrowed kind's nodes
// that a removal in the same plan deletes aren't in the way.
func (s *Store) PlanCounts(ctx context.Context, req PlanRequest) (*PlanCounts, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "plan_counts", 4*dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if slices.Contains(req.KindsRemoved, "") {
		return nil, op.Expected("invalid", errors.FieldError("kinds_removed", errors.FallbackValidation))
	}
	for _, n := range req.ParentsNarrowed {
		if n.Kind == "" || slices.Contains(n.Removed, "") {
			return nil, op.Expected("invalid", errors.FieldError("parents_narrowed", errors.FallbackValidation))
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	out := &PlanCounts{KindsRemoved: map[string]Effects{}, ParentsNarrowed: map[string]InTheWay{}}
	now := s.now().UTC()
	for _, kind := range req.KindsRemoved {
		var e Effects
		if err := tx.QueryRow(ctx, `
			WITH gone AS (
				SELECT DISTINCT n.id FROM nodes t
				JOIN node_ancestors a ON a.ancestor_id = t.id
				JOIN nodes n ON n.id = a.node_id
				WHERE t.kind = $1 AND n.status <> 'purged'
			), sas AS (
				SELECT p.id FROM principals p WHERE p.born_node_id IN (SELECT id FROM gone)
			)
			SELECT
				(SELECT count(*) FROM gone),
				(SELECT count(*) FROM bindings b
				 WHERE (b.expires_at IS NULL OR b.expires_at > $2)
				   AND (b.node_id IN (SELECT id FROM gone)
				        OR b.member_id IN (SELECT m.id FROM members m WHERE m.principal_id IN (SELECT id FROM sas)))),
				(SELECT count(*) FROM sas),
				(SELECT count(*) FROM api_keys k WHERE k.principal_id IN (SELECT id FROM sas) AND k.revoked_at IS NULL)
		`, kind, now).Scan(&e.Nodes, &e.Bindings, &e.ServiceAccounts, &e.Keys); err != nil {
			return nil, op.Fail(err)
		}
		out.KindsRemoved[kind] = e
	}
	removed := req.KindsRemoved
	if removed == nil {
		removed = []string{}
	}
	for _, n := range req.ParentsNarrowed {
		w := InTheWay{Sample: []string{}}
		var sample []string
		if err := tx.QueryRow(ctx, `
			SELECT count(*), COALESCE((array_agg(n.id ORDER BY n.id))[1:$4], '{}')
			FROM nodes n JOIN nodes p ON p.id = n.parent_id
			WHERE n.kind = $1 AND p.kind = ANY($2) AND n.status <> 'purged'
			  AND NOT EXISTS (
			      SELECT 1 FROM node_ancestors a JOIN nodes t ON t.id = a.ancestor_id
			      WHERE a.node_id = n.id AND t.kind = ANY($3))
		`, n.Kind, n.Removed, removed, planSample).Scan(&w.Nodes, &sample); err != nil {
			return nil, op.Fail(err)
		}
		if sample != nil {
			w.Sample = sample
		}
		out.ParentsNarrowed[n.Kind] = w
	}
	op.OK("ok")
	return out, nil
}

// MountInternal serves plan counts on the internal port.
func (h *Handler) MountInternal(r fiber.Router) {
	r.Post("/plan", h.PlanCounts)
}

func (h *Handler) PlanCounts(c fiber.Ctx) error {
	var req PlanRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	out, err := h.store.PlanCounts(c.Context(), req)
	if err != nil {
		return httpx.MapDB(c.Context(), err, "failed to count plan", httpx.DBErr{})
	}
	return c.JSON(out)
}
