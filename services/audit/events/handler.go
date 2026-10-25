package events

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/audit/errors"
	"github.com/plat5dev/plat5/audit/internal/httpx"
	"github.com/plat5dev/plat5/audit/internal/id"
	"github.com/plat5dev/plat5/audit/metrics"
)

type store interface {
	CreateIntent(ctx context.Context, in *Intent, final *Outcome) (bool, error)
	ApplyOutcome(ctx context.Context, requestID string, out *Outcome) (string, error)
	List(ctx context.Context, f Filter) ([]*Event, bool, error)
}

type Handler struct {
	store store
}

func NewHandler(s *Store) *Handler {
	return &Handler{store: s}
}

// MountInternal registers the gateway's writes on a router scoped under /internal.
func (h *Handler) MountInternal(router fiber.Router) {
	router.Put("/events/:request_id", h.PutIntent)
	router.Patch("/events/:request_id", h.PatchOutcome)
}

// MountPublic registers the reads. Audit's resources publish them as
// …/audit-events at every kind and /user/audit-events
// (docs/reference/api/audit.md#reading).
func (h *Handler) MountPublic(router fiber.Router) {
	router.Get("/nodes/:node_id/audit-events", h.ListNode)
	router.Get("/users/:user_id/audit-events", h.ListUser)
}

// PutIntent creates the event, pending or, for a gateway refusal, final. An
// existing one is left alone.
func (h *Handler) PutIntent(c fiber.Ctx) error {
	in, final, err := ParseIntent(httpx.PathParam(c, "request_id"), c.Body())
	if err != nil {
		return err
	}
	created, err := h.store.CreateIntent(c.Context(), in, final)
	if err != nil {
		httpx.LogError(c.Context(), "create audit intent", err, errors.KindDB)
		return errors.ServiceUnavailableError()
	}
	if !created {
		metrics.RecordIntent(metrics.IntentExisting)
		return c.SendStatus(fiber.StatusOK)
	}
	metrics.RecordIntent(metrics.IntentCreated)
	return c.SendStatus(fiber.StatusCreated)
}

// PatchOutcome applies the outcome once. A final event is left unchanged.
func (h *Handler) PatchOutcome(c fiber.Ctx) error {
	requestID := httpx.PathParam(c, "request_id")
	out, err := ParseOutcome(requestID, c.Body())
	if err != nil {
		return err
	}
	result, err := h.store.ApplyOutcome(c.Context(), requestID, out)
	if stderrors.Is(err, ErrNotFound) {
		metrics.RecordOutcome(metrics.OutcomeNotFound)
		return errors.NotFoundError("audit_event", requestID)
	}
	if err != nil {
		httpx.LogError(c.Context(), "apply audit outcome", err, errors.KindDB)
		return errors.ServiceUnavailableError()
	}
	metrics.RecordOutcome(result)
	return c.SendStatus(fiber.StatusNoContent)
}

type listResponse struct {
	AuditEvents []eventResponse `json:"audit_events"`
	HasMore     bool            `json:"has_more"`
}

type eventResponse struct {
	ID            string            `json:"id"`
	OccurredAt    string            `json:"occurred_at"`
	RequestID     string            `json:"request_id"`
	RootID        *string           `json:"root_id"`
	Nodes         []Node            `json:"nodes"`
	Actor         Actor             `json:"actor"`
	Service       string            `json:"service"`
	Method        *string           `json:"method"`
	Route         *string           `json:"route"`
	Params        map[string]string `json:"params"`
	IP            *string           `json:"ip"`
	UserAgent     *string           `json:"user_agent"`
	Outcome       string            `json:"outcome"`
	Status        *int              `json:"status"`
	Details       json.RawMessage   `json:"details"`
	CreatedID     *string           `json:"created_id"`
	NamedNodes    []string          `json:"named_nodes"`
	NamedMemberID *string           `json:"named_member_id"`
}

func toResponse(e *Event) eventResponse {
	nodes := e.Nodes
	if nodes == nil {
		nodes = []Node{}
	}
	params := e.Params
	if params == nil {
		params = map[string]string{}
	}
	details := e.Details
	if details == nil {
		details = json.RawMessage("null")
	}
	named := e.NamedNodes
	if named == nil {
		named = []string{}
	}
	return eventResponse{
		ID:            e.ID,
		OccurredAt:    e.OccurredAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		RequestID:     e.RequestID,
		RootID:        e.RootID,
		Nodes:         nodes,
		Actor:         e.Actor,
		Service:       e.Service,
		Method:        e.Method,
		Route:         e.Route,
		Params:        params,
		IP:            e.IP,
		UserAgent:     e.UserAgent,
		Outcome:       e.Outcome.Outcome,
		Status:        e.Status,
		Details:       details,
		CreatedID:     e.CreatedID,
		NamedNodes:    named,
		NamedMemberID: e.NamedMemberID,
	}
}

// ListNode is one page of a node's log, newest first: the events on it and
// below it, those that named it, and the one that created it. Audit doesn't
// know which nodes exist: one with no events is an empty list.
func (h *Handler) ListNode(c fiber.Ctx) error {
	f, err := parseFilter(c)
	if err != nil {
		return err
	}
	f.Log, f.NodeRead = httpx.PathParam(c, "node_id"), true
	return h.list(c, f)
}

// ListUser is one page of a person's own log, their user routes' events,
// newest first.
func (h *Handler) ListUser(c fiber.Ctx) error {
	f, err := parseFilter(c)
	if err != nil {
		return err
	}
	f.Log = UserLog(httpx.PathParam(c, "user_id"))
	return h.list(c, f)
}

func (h *Handler) list(c fiber.Ctx, f Filter) error {
	list, hasMore, err := h.store.List(c.Context(), f)
	if err != nil {
		httpx.LogError(c.Context(), "list audit events", err, errors.KindDB)
		return errors.InternalError()
	}
	resp := listResponse{AuditEvents: make([]eventResponse, 0, len(list)), HasMore: hasMore}
	for _, e := range list {
		resp.AuditEvents = append(resp.AuditEvents, toResponse(e))
	}
	return c.JSON(resp)
}

// parseFilter reads the list query. Unknown, repeated, or malformed params are
// 422 (docs/reference/api/audit.md#reading).
func parseFilter(c fiber.Ctx) (Filter, error) {
	f := Filter{Limit: httpx.DefaultListLimit}
	var bad fieldErrs
	seen := map[string]bool{}

	for k, v := range c.Request().URI().QueryArgs().All() {
		key, value := string(k), strings.TrimSpace(string(v))
		if seen[key] {
			bad.add(key)
			continue
		}
		seen[key] = true

		if name, ok := paramFilterName(key); ok {
			if !validParamName(name) || value == "" || len(value) > maxTextLen {
				bad.add(key)
				continue
			}
			if f.Params == nil {
				f.Params = map[string]string{}
			}
			f.Params[name] = value
			continue
		}

		switch key {
		case "limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				bad.add(key)
				continue
			}
			f.Limit = min(n, httpx.MaxListLimit)
		case "starting_after":
			if !id.Valid(value) {
				bad.add(key)
				continue
			}
			f.StartingAfter = value
		case "actor_member_id":
			f.ActorMemberID = filterText(&bad, key, value)
		case "service":
			f.Service = filterText(&bad, key, value)
		case "method":
			f.Method = filterText(&bad, key, value)
		case "route":
			f.Route = filterText(&bad, key, value)
		case "outcome":
			f.Outcome = filterText(&bad, key, value)
		case "status":
			n, err := strconv.Atoi(value)
			if err != nil {
				bad.add(key)
				continue
			}
			f.Status = &n
		case "request_id":
			f.RequestID = filterText(&bad, key, value)
		case "created_id":
			f.CreatedID = filterText(&bad, key, value)
		case "occurred_after", "occurred_before":
			t, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				bad.add(key)
				continue
			}
			if key == "occurred_after" {
				f.OccurredAfter = &t
			} else {
				f.OccurredBefore = &t
			}
		default:
			bad.add(key)
		}
	}
	if err := bad.err(); err != nil {
		return Filter{}, err
	}
	return f, nil
}

// paramFilterName is name for a "params[name]" key.
func paramFilterName(key string) (string, bool) {
	if !strings.HasPrefix(key, "params[") || !strings.HasSuffix(key, "]") {
		return "", false
	}
	return key[len("params[") : len(key)-1], true
}

const maxTextLen = 1024

type fieldErrs []errors.Field

func (f *fieldErrs) add(path string) {
	*f = append(*f, errors.Field{Path: path, Message: errors.FallbackValidation})
}

func (f fieldErrs) err() error {
	if len(f) == 0 {
		return nil
	}
	return errors.ValidationFields("", f...)
}

// filterText is an exact-match value: 1..maxTextLen bytes of printable ASCII.
func filterText(bad *fieldErrs, key, value string) string {
	if value == "" || len(value) > maxTextLen || !printableASCII(value) {
		bad.add(key)
		return ""
	}
	return value
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// validParamName is a route param's name in a params[<name>] filter.
func validParamName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
