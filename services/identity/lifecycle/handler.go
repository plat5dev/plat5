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
	ListTree(ctx context.Context, limit int, startingAfter int64) ([]*TreeEvent, bool, error)
	ListUsers(ctx context.Context, limit int, startingAfter int64) ([]*UserEvent, bool, error)
}

type Handler struct {
	store eventStore
}

func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

type TreeEventResponse struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Kind       *string `json:"kind,omitempty"`
	NodeID     *string `json:"node_id,omitempty"`
	MemberID   *string `json:"member_id,omitempty"`
	RootID     string  `json:"root_id"`
	Principal  *string `json:"principal,omitempty"`
	OccurredAt string  `json:"occurred_at"`
}

type TreeListResponse struct {
	TreeEvents []TreeEventResponse `json:"tree_events"`
	HasMore    bool                `json:"has_more"`
}

// UserEventResponse is every field a user event has: no tree, no member, no
// name.
type UserEventResponse struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	UserID     string `json:"user_id"`
	OccurredAt string `json:"occurred_at"`
}

type UserListResponse struct {
	UserEvents []UserEventResponse `json:"user_events"`
	HasMore    bool                `json:"has_more"`
}

func (h *Handler) MountInternal(r fiber.Router) {
	r.Get("/tree-events", h.ListTree)
	r.Get("/user-events", h.ListUsers)
}

// ListTree is the tree events feed. Pagination as docs/reference/lists.md,
// except starting_after is an event id: a decimal string, not a ULID.
func (h *Handler) ListTree(c fiber.Ctx) error {
	ctx := c.Context()
	limit, after, err := parsePage(c)
	if err != nil {
		return err
	}
	events, hasMore, err := h.store.ListTree(ctx, limit, after)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list tree events", httpx.DBErr{})
	}

	out := TreeListResponse{TreeEvents: make([]TreeEventResponse, 0, len(events)), HasMore: hasMore}
	for _, e := range events {
		out.TreeEvents = append(out.TreeEvents, TreeEventResponse{
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

// ListUsers is the user events feed, paged as the tree events feed.
func (h *Handler) ListUsers(c fiber.Ctx) error {
	ctx := c.Context()
	limit, after, err := parsePage(c)
	if err != nil {
		return err
	}
	events, hasMore, err := h.store.ListUsers(ctx, limit, after)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list user events", httpx.DBErr{})
	}

	out := UserListResponse{UserEvents: make([]UserEventResponse, 0, len(events)), HasMore: hasMore}
	for _, e := range events {
		out.UserEvents = append(out.UserEvents, UserEventResponse{
			ID:         strconv.FormatInt(e.ID, 10),
			Type:       e.Type,
			UserID:     e.UserID,
			OccurredAt: httpx.FormatTime(e.OccurredAt),
		})
	}
	return c.JSON(out)
}

func parsePage(c fiber.Ctx) (int, int64, error) {
	limit, err := httpx.ParseLimit(c)
	if err != nil {
		return 0, 0, err
	}
	after, err := parseStartingAfter(c.Query("starting_after"))
	if err != nil {
		return 0, 0, err
	}
	return limit, after, nil
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
