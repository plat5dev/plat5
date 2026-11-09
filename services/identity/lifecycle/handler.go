package lifecycle

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

// LongPollWait is how long a read with nothing to answer waits for a newer
// event before answering 204.
const LongPollWait = 25 * time.Second

type eventStore interface {
	Head(ctx context.Context) (int64, error)
	List(ctx context.Context, after int64, limit int, types []string) ([]*Event, bool, error)
}

type Handler struct {
	store eventStore
	head  *Head
	wait  time.Duration
}

// NewHandler serves the log from store. A read with nothing to answer waits
// up to wait; Follow must run for waiting readers to wake.
func NewHandler(store *Store, wait time.Duration) *Handler {
	return &Handler{store: store, head: NewHead(), wait: wait}
}

// Follow reads the log's head every interval until ctx ends, waking waiting
// readers when it moves.
func (h *Handler) Follow(ctx context.Context, interval time.Duration, log zerolog.Logger) {
	h.head.Follow(ctx, h.store.Head, interval, log)
}

// EventResponse is one event. A field the event's type doesn't carry is
// absent.
type EventResponse struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Kind       *string `json:"kind,omitempty"`
	NodeID     *string `json:"node_id,omitempty"`
	MemberID   *string `json:"member_id,omitempty"`
	RootID     *string `json:"root_id,omitempty"`
	Principal  *string `json:"principal,omitempty"`
	UserID     *string `json:"user_id,omitempty"`
	OccurredAt string  `json:"occurred_at"`
}

type ListResponse struct {
	Events  []EventResponse `json:"events"`
	HasMore bool            `json:"has_more"`
}

// Path is the log's path on the internal port. It's mounted on its own,
// off traces and request metrics.
const Path = "/internal/events"

// List is GET /internal/events (docs/reference/api/internal.md#lifecycle-events).
func (h *Handler) List(c fiber.Ctx) error {
	ctx := c.Context()
	q, err := parseQuery(c)
	if err != nil {
		return err
	}
	// With or without after, nothing to answer is a wait, then 204: a 200
	// always has an event, so a reader asks again at once whatever came.
	wait, cancel := context.WithTimeout(ctx, h.wait)
	defer cancel()
	for {
		// The head first: every event up to it has committed, so a list
		// read after it that finds nothing means nothing up to it matches.
		head, err := h.store.Head(ctx)
		if err != nil {
			return httpx.MapDB(ctx, err, "failed to read the events head", httpx.DBErr{})
		}
		h.head.Advance(head)
		if q.after > head {
			// Past the newest event: identity's log was reset, and its new
			// events have ids below the cursor. The reader starts over.
			return errors.CursorAheadError()
		}
		events, more, err := h.store.List(ctx, q.after, q.limit, q.types)
		if err != nil {
			return httpx.MapDB(ctx, err, "failed to list events", httpx.DBErr{})
		}
		if len(events) > 0 {
			return c.JSON(response(events, more))
		}
		if !h.head.WaitPast(wait, head) {
			return c.SendStatus(fiber.StatusNoContent)
		}
	}
}

func response(events []*Event, more bool) ListResponse {
	out := ListResponse{Events: make([]EventResponse, 0, len(events)), HasMore: more}
	for _, e := range events {
		out.Events = append(out.Events, EventResponse{
			ID:         strconv.FormatInt(e.ID, 10),
			Type:       e.Type,
			Kind:       e.Kind,
			NodeID:     e.NodeID,
			MemberID:   e.MemberID,
			RootID:     e.RootID,
			Principal:  e.Principal,
			UserID:     e.UserID,
			OccurredAt: httpx.FormatTime(e.OccurredAt),
		})
	}
	return out
}

type query struct {
	after    int64 // 0: from the first event
	limit    int
	types    []string // nil: every type
}

func parseQuery(c fiber.Ctx) (query, error) {
	var q query
	limit, err := httpx.ParseLimit(c)
	if err != nil {
		return q, err
	}
	q.limit = limit
	args := c.Request().URI().QueryArgs()
	if args.Has("after") {
		after, ok := parseID(string(args.Peek("after")))
		if !ok {
			return q, errors.FieldError("after", errors.FallbackValidation)
		}
		q.after = after
	}
	if args.Has("types") {
		for _, t := range strings.Split(string(args.Peek("types")), ",") {
			if !slices.Contains(Types, t) {
				return q, errors.FieldError("types", errors.FallbackValidation)
			}
			q.types = append(q.types, t)
		}
	}
	return q, nil
}

// parseID accepts a positive decimal integer with no leading zero.
func parseID(v string) (int64, bool) {
	if v == "" || v[0] < '1' || v[0] > '9' {
		return 0, false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}
