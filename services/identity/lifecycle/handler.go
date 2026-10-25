package lifecycle

import (
	"context"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

type eventStore interface {
	List(ctx context.Context, limit int, startingAfter int64) ([]*Event, bool, error)
}

type Handler struct {
	store eventStore
}

func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

type EventResponse struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Kind       *string `json:"kind,omitempty"`
	NodeID     *string `json:"node_id,omitempty"`
	MemberID   *string `json:"member_id,omitempty"`
	RootID     string  `json:"root_id"`
	Principal  *string `json:"principal,omitempty"`
	OccurredAt string  `json:"occurred_at"`
}

type ListResponse struct {
	LifecycleEvents []EventResponse `json:"lifecycle_events"`
	HasMore         bool            `json:"has_more"`
}

func (h *Handler) MountInternal(r fiber.Router) {
	r.Get("/lifecycle-events", h.List)
}

// List is the feed. Pagination as docs/reference/lists.md, except starting_after is an
// event id: a decimal string, not a ULID.
func (h *Handler) List(c fiber.Ctx) error {
	ctx := c.Context()
	limit, err := httpx.ParseLimit(c)
	if err != nil {
		return err
	}
	after, err := parseStartingAfter(c.Query("starting_after"))
	if err != nil {
		return err
	}

	events, hasMore, err := h.store.List(ctx, limit, after)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list lifecycle events", httpx.DBErr{})
	}

	out := ListResponse{LifecycleEvents: make([]EventResponse, 0, len(events)), HasMore: hasMore}
	for _, e := range events {
		out.LifecycleEvents = append(out.LifecycleEvents, EventResponse{
			ID:         strconv.FormatInt(e.ID, 10),
			Type:       e.Type,
			Kind:       e.Kind,
			NodeID:     e.NodeID,
			MemberID:   e.MemberID,
			RootID:     e.RootID,
			Principal:  e.Principal,
			OccurredAt: httpx.FormatTime(e.OccurredAt),
		})
	}
	return c.JSON(out)
}

// parseStartingAfter accepts "" (from the first event) or a positive decimal id.
func parseStartingAfter(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	if v[0] < '1' || v[0] > '9' {
		return 0, errors.FieldError("starting_after", errors.FallbackValidation)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, errors.FieldError("starting_after", errors.FallbackValidation)
	}
	return n, nil
}
