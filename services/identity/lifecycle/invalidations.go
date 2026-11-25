package lifecycle

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

// Invalidation scopes: every target is an id the gateway already receives from
// admit. A principal id never appears here.
const (
	ScopeKey    = "key"
	ScopeUser   = "user"
	ScopeMember = "member"
	ScopeRoot   = "root"

	InvalidationsPath     = "/internal/admit-invalidations"
	defaultInvalidLimit   = 500
	maxInvalidLimit       = 1000
	invalidationRetention = 24 * time.Hour
)

// Invalidation is one commit-ordered cache invalidation entry.
type Invalidation struct {
	ID     int64  `json:"id"`
	Scope  string `json:"scope"`
	Target string `json:"target"`
}

func InvalidateKey(keyID string) Invalidation {
	return Invalidation{Scope: ScopeKey, Target: keyID}
}

func InvalidateUser(userID string) Invalidation {
	return Invalidation{Scope: ScopeUser, Target: userID}
}

func InvalidateMember(memberID string) Invalidation {
	return Invalidation{Scope: ScopeMember, Target: memberID}
}

func InvalidateRoot(rootID string) Invalidation {
	return Invalidation{Scope: ScopeRoot, Target: rootID}
}

// InvalidationHeadSQL evaluates the effective admit invalidations head: the
// greater of the newest retained row id and the pruned watermark, so pruning
// all rows never moves head backwards below pruned_through.
const InvalidationHeadSQL = `GREATEST(COALESCE((SELECT MAX(id) FROM admit_invalidations), 0), COALESCE((SELECT pruned_through FROM admit_invalidation_watermark WHERE id = 1), 0))`

func queueInvalidations(batch *pgx.Batch, invs []Invalidation) bool {
	if len(invs) == 0 {
		return false
	}
	if len(invs) == 1 {
		batch.Queue(`INSERT INTO admit_invalidations (scope, target) VALUES ($1, $2) RETURNING id`, invs[0].Scope, invs[0].Target)
		return true
	}
	type pair struct{ scope, target string }
	seen := make(map[pair]struct{}, len(invs))
	scopes := make([]string, 0, len(invs))
	targets := make([]string, 0, len(invs))
	for _, inv := range invs {
		k := pair{inv.Scope, inv.Target}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		scopes = append(scopes, inv.Scope)
		targets = append(targets, inv.Target)
	}
	batch.Queue(`
		WITH ins AS (
			INSERT INTO admit_invalidations (scope, target)
			SELECT u.scope, u.target
			FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS u(scope, target, ord)
			ORDER BY u.ord
			RETURNING id
		)
		SELECT MAX(id) FROM ins
	`, scopes, targets)
	return true
}

// InvalidationStore reads and prunes the admit_invalidations table.
type InvalidationStore struct {
	pool *pgxpool.Pool
}

func NewInvalidationStore(pool *pgxpool.Pool) *InvalidationStore {
	return &InvalidationStore{pool: pool}
}

// Head returns the effective newest committed invalidation id (never below the
// pruned watermark, 0 when nothing has ever been written).
func (s *InvalidationStore) Head(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, dbx.DefaultTimeout)
	defer cancel()
	var head int64
	err := s.pool.QueryRow(ctx, `SELECT `+InvalidationHeadSQL).Scan(&head)
	return head, err
}

// State returns (head, prunedThrough) from one Postgres query. Head is always
// at least prunedThrough so full retention pruning never causes a reset loop.
func (s *InvalidationStore) State(ctx context.Context) (head, prunedThrough int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, dbx.DefaultTimeout)
	defer cancel()
	err = s.pool.QueryRow(ctx, `
		WITH w AS (SELECT COALESCE((SELECT pruned_through FROM admit_invalidation_watermark WHERE id = 1), 0) AS pruned)
		SELECT GREATEST(COALESCE((SELECT MAX(id) FROM admit_invalidations), 0), w.pruned), w.pruned
		FROM w
	`).Scan(&head, &prunedThrough)
	return head, prunedThrough, err
}

// List returns invalidations with id > after, ascending, up to limit.
func (s *InvalidationStore) List(ctx context.Context, after int64, limit int) ([]Invalidation, error) {
	ctx, cancel := context.WithTimeout(ctx, dbx.DefaultTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT id, scope, target
		FROM admit_invalidations
		WHERE id > $1
		ORDER BY id
		LIMIT $2
	`, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invalidation, error) {
		var inv Invalidation
		err := r.Scan(&inv.ID, &inv.Scope, &inv.Target)
		return inv, err
	})
}

// Prune deletes invalidation rows older than retention and advances the
// watermark inside one transaction. Returns how many rows were removed.
func (s *InvalidationStore) Prune(ctx context.Context) (int, error) {
	return s.PruneOlderThan(ctx, invalidationRetention)
}

func (s *InvalidationStore) PruneOlderThan(ctx context.Context, age time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*dbx.DefaultTimeout)
	defer cancel()
	cutoff := time.Now().UTC().Add(-age)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var count int
	var maxID *int64
	if err := tx.QueryRow(ctx, `
		WITH gone AS (
			DELETE FROM admit_invalidations
			WHERE created_at < $1
			RETURNING id
		)
		SELECT count(*), MAX(id) FROM gone
	`, cutoff).Scan(&count, &maxID); err != nil {
		return 0, err
	}
	if maxID != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO admit_invalidation_watermark (id, pruned_through) VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET pruned_through = GREATEST(admit_invalidation_watermark.pruned_through, EXCLUDED.pruned_through)
		`, *maxID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

// ConfigRevisionSource provides identity's current control-plane revision and
// notifies when it changes.
type ConfigRevisionSource interface {
	Revision() int64
	Changed() <-chan struct{}
}

type invalidationStoreIface interface {
	Head(ctx context.Context) (int64, error)
	State(ctx context.Context) (head, prunedThrough int64, err error)
	List(ctx context.Context, after int64, limit int) ([]Invalidation, error)
}

// InvalidationHandler serves GET /internal/admit-invalidations
// (docs/reference/api/internal.md#admit-invalidations).
type InvalidationHandler struct {
	store  invalidationStoreIface
	config ConfigRevisionSource
	head   *Head
	wait   time.Duration
}

func NewInvalidationHandler(store *InvalidationStore, config ConfigRevisionSource, wait time.Duration) *InvalidationHandler {
	return &InvalidationHandler{store: store, config: config, head: NewHead(), wait: wait}
}

// Follow polls the invalidations head for multi-replica writes until ctx ends.
func (h *InvalidationHandler) Follow(ctx context.Context, interval time.Duration, log zerolog.Logger) {
	h.head.Follow(ctx, h.store.Head, interval, log)
}

// Notify wakes local long-poll readers immediately after a local write commits
// invalidations up to head, without issuing an extra database query.
func (h *InvalidationHandler) Notify(head int64) {
	if head > 0 {
		h.head.Advance(head)
	}
}

type InvalidationResponse struct {
	Head           int64          `json:"head"`
	ConfigRevision int64          `json:"config_revision"`
	Reset          bool           `json:"reset"`
	Events         []Invalidation `json:"events"`
}

func (h *InvalidationHandler) List(c fiber.Ctx) error {
	ctx := c.Context()
	args := c.Request().URI().QueryArgs()

	hasAfter := args.Has("after")
	var after int64
	if hasAfter {
		n, ok := parseNonNegInt(string(args.Peek("after")))
		if !ok {
			return errors.FieldError("after", errors.FallbackValidation)
		}
		after = n
	}

	hasConfigRev := args.Has("config_revision")
	var clientConfigRev int64
	if hasConfigRev {
		n, ok := parseNonNegInt(string(args.Peek("config_revision")))
		if !ok {
			return errors.FieldError("config_revision", errors.FallbackValidation)
		}
		clientConfigRev = n
	}

	limit := defaultInvalidLimit
	if args.Has("limit") {
		n, ok := parseID(string(args.Peek("limit")))
		if !ok {
			return errors.FieldError("limit", errors.FallbackValidation)
		}
		limit = int(min(n, maxInvalidLimit))
	}

	currentRev := int64(0)
	if h.config != nil {
		currentRev = h.config.Revision()
	}

	// Without after: immediate initial sync returning current head & config_revision.
	if !hasAfter {
		head, _, err := h.store.State(ctx)
		if err != nil {
			return httpx.MapDB(ctx, err, "failed to read invalidations head", httpx.DBErr{})
		}
		h.head.Sync(head)
		return c.JSON(InvalidationResponse{
			Head:           head,
			ConfigRevision: currentRev,
			Reset:          false,
			Events:         []Invalidation{},
		})
	}

	wait, cancel := context.WithTimeout(ctx, h.wait)
	defer cancel()
	for {
		var revChanged <-chan struct{}
		if h.config != nil {
			revChanged = h.config.Changed()
			currentRev = h.config.Revision()
		}
		head, prunedThrough, err := h.store.State(ctx)
		if err != nil {
			return httpx.MapDB(ctx, err, "failed to read invalidations state", httpx.DBErr{})
		}
		h.head.Sync(head)
		// Head may be 0 if all rows were pruned; if cursor is ahead of head or
		// behind pruned_through, signal a full cache reset.
		if after > head || after < prunedThrough {
			return c.JSON(InvalidationResponse{
				Head:           head,
				ConfigRevision: currentRev,
				Reset:          true,
				Events:         []Invalidation{},
			})
		}
		events, err := h.store.List(ctx, after, limit)
		if err != nil {
			return httpx.MapDB(ctx, err, "failed to list invalidations", httpx.DBErr{})
		}
		if len(events) > 0 || (hasConfigRev && currentRev != clientConfigRev) {
			if events == nil {
				events = []Invalidation{}
			}
			return c.JSON(InvalidationResponse{
				Head:           head,
				ConfigRevision: currentRev,
				Reset:          false,
				Events:         events,
			})
		}
		if !h.head.WaitPastWith(wait, head, revChanged) {
			return c.SendStatus(fiber.StatusNoContent)
		}
	}
}

func parseNonNegInt(v string) (int64, bool) {
	if v == "0" {
		return 0, true
	}
	return parseID(v)
}
