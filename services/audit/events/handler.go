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
	CreateIntent(ctx context.Context, in *Intent) (bool, error)
	ApplyOutcome(ctx context.Context, requestID string, out *Outcome) (string, error)
	List(ctx context.Context, f Filter) ([]*Event, bool, error)
}

type Handler struct {
	store store
	now   func() time.Time
}

func NewHandler(s *Store) *Handler {
	return &Handler{store: s, now: time.Now}
}

// MountInternal registers the gateway's writes on a router scoped under /internal.
func (h *Handler) MountInternal(router fiber.Router) {
	router.Put("/events/:request_id", h.PutIntent)
	router.Patch("/events/:request_id", h.PatchOutcome)
}

// MountPublic registers the org's read. The catalog publishes it as /org/audit-events.
func (h *Handler) MountPublic(router fiber.Router) {
	router.Get("/organizations/:organization_id/audit-events", h.List)
}

// PutIntent creates the pending event, or leaves an existing one alone.
func (h *Handler) PutIntent(c fiber.Ctx) error {
	in, err := ParseIntent(httpx.PathParam(c, "request_id"), c.Body(), h.now())
	if err != nil {
		return err
	}
	created, err := h.store.CreateIntent(c.Context(), in)
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
	ID             string            `json:"id"`
	OccurredAt     string            `json:"occurred_at"`
	RequestID      string            `json:"request_id"`
	OrganizationID string            `json:"organization_id"`
	Actor          Actor             `json:"actor"`
	Service        string            `json:"service"`
	Method         string            `json:"method"`
	Route          string            `json:"route"`
	Params         map[string]string `json:"params"`
	IP             string            `json:"ip"`
	UserAgent      *string           `json:"user_agent"`
	Outcome        string            `json:"outcome"`
	Status         *int              `json:"status"`
	Details        json.RawMessage   `json:"details"`
}

func toResponse(e *Event) eventResponse {
	params := e.Params
	if params == nil {
		params = map[string]string{}
	}
	details := e.Details
	if details == nil {
		details = json.RawMessage("null")
	}
	return eventResponse{
		ID:             e.ID,
		OccurredAt:     e.OccurredAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		RequestID:      e.RequestID,
		OrganizationID: e.OrganizationID,
		Actor:          e.Actor,
		Service:        e.Service,
		Method:         e.Method,
		Route:          e.Route,
		Params:         params,
		IP:             e.IP,
		UserAgent:      e.UserAgent,
		Outcome:        e.Outcome.Outcome,
		Status:         e.Status,
		Details:        details,
	}
}

// List is one page of the org's log, newest first.
func (h *Handler) List(c fiber.Ctx) error {
	f, err := parseFilter(c)
	if err != nil {
		return err
	}
	f.OrganizationID = httpx.PathParam(c, "organization_id")

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

var listOutcomes = map[string]bool{
	OutcomePending: true, OutcomeRejected: true, OutcomeResponded: true, OutcomeNoResponse: true,
}

// parseFilter reads the list query. Unknown, repeated, or malformed params are
// 422 (docs/audit.md#reading).
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
			if !validParamName(name) || value == "" || len(value) > maxParamValueLen {
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
			f.ActorMemberID = filterText(&bad, key, value, maxIDLen)
		case "service":
			f.Service = filterText(&bad, key, value, maxServiceLen)
		case "method":
			if !auditedMethods[value] {
				bad.add(key)
				continue
			}
			f.Method = value
		case "route":
			f.Route = filterText(&bad, key, value, maxRouteLen)
		case "outcome":
			if !listOutcomes[value] {
				bad.add(key)
				continue
			}
			f.Outcome = value
		case "request_id":
			if !ValidRequestID(value) {
				bad.add(key)
				continue
			}
			f.RequestID = value
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

func filterText(bad *fieldErrs, key, value string, max int) string {
	if value == "" || len(value) > max || !printableASCII(value) {
		bad.add(key)
		return ""
	}
	return value
}
