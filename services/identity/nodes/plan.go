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

// planSample is how many ids a kind's count shows.
const planSample = 5

// PlanRequest is POST /internal/plan: the kinds a plan removes or moves
// under another parent (docs/reference/api/internal.md#plan-counts).
type PlanRequest struct {
	Kinds []string `json:"kinds"`
}

// KindCount is what's in the way of changing a kind: its live nodes, its live
// and deleted ones, and a few of their ids, live ones first. Purged nodes
// never count.
type KindCount struct {
	Live   int64    `json:"live"`
	Nodes  int64    `json:"nodes"`
	Sample []string `json:"sample"`
}

type PlanCounts struct {
	Kinds map[string]KindCount `json:"kinds"`
}

// PlanCounts counts each kind's nodes, in one snapshot.
func (s *Store) PlanCounts(ctx context.Context, req PlanRequest) (*PlanCounts, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "plan_counts", 4*dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if slices.Contains(req.Kinds, "") {
		return nil, op.Expected("invalid", errors.FieldError("kinds", errors.FallbackValidation))
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, op.Fail(err)
	}
	defer tx.Rollback(ctx)

	out := &PlanCounts{Kinds: map[string]KindCount{}}
	for _, kind := range req.Kinds {
		c := KindCount{Sample: []string{}}
		var sample []string
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE status = 'active'), count(*),
			       COALESCE((array_agg(id ORDER BY status <> 'active', id))[1:$2], '{}')
			FROM nodes WHERE kind = $1 AND status <> 'purged'
		`, kind, planSample).Scan(&c.Live, &c.Nodes, &sample); err != nil {
			return nil, op.Fail(err)
		}
		if sample != nil {
			c.Sample = sample
		}
		out.Kinds[kind] = c
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
