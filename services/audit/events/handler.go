package events

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/audit/errors"
	"github.com/plat5dev/plat5/audit/internal/dbx"
	"github.com/plat5dev/plat5/audit/internal/httpx"
	"github.com/plat5dev/plat5/audit/internal/id"
	"github.com/plat5dev/plat5/audit/metrics"
)

const (
	defaultExportLimit = 100
	maxExportLimit     = 1000
)

type store interface {
	CreateIntent(ctx context.Context, in *Intent, final *Outcome) (bool, error)
	ApplyOutcome(ctx context.Context, requestID string, out *Outcome) (string, error)
	List(ctx context.Context, f Filter) ([]*Event, bool, error)
	Export(ctx context.Context, after int64, limit int) ([]*Event, int64, bool, error)
}

type Handler struct {
	store store
}

func NewHandler(s *Store) *Handler {
	return &Handler{store: s}
}

// MountInternal registers the gateway's writes and the deployment-wide export
// feed on a router scoped under /internal.
func (h *Handler) MountInternal(router fiber.Router) {
	router.Get("/events", h.Export)
	router.Put("/events/:request_id", h.PutIntent)
	router.Patch("/events/:request_id", h.PatchOutcome)
}

// MountPublic registers the reads. services/audit.yml publishes them as
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
		return dbError(c.Context(), "create audit intent", err)
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
		return dbError(c.Context(), "apply audit outcome", err)
	}
	metrics.RecordOutcome(result)
	return c.SendStatus(fiber.StatusNoContent)
}

type listResponse struct {
	AuditEvents []eventResponse `json:"audit_events"`
	HasMore     bool            `json:"has_more"`
}

type exportEventResponse struct {
	Seq string `json:"seq"`
	eventResponse
}

type exportResponse struct {
	AuditEvents []exportEventResponse `json:"audit_events"`
	Cursor      string                `json:"cursor"`
	HasMore     bool                  `json:"has_more"`
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
	NamedNodeIDs  []string          `json:"named_node_ids"`
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
	named := e.NamedNodeIDs
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
		NamedNodeIDs:  named,
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
		return dbError(c.Context(), "list audit events", err)
	}
	resp := listResponse{AuditEvents: make([]eventResponse, 0, len(list)), HasMore: hasMore}
	for _, e := range list {
		resp.AuditEvents = append(resp.AuditEvents, toResponse(e))
	}
	return c.JSON(resp)
}

// Export is GET /internal/events: deployment-wide finalized audit events in
// commit order for SIEM / WORM archival (docs/reference/api/internal.md#export).
func (h *Handler) Export(c fiber.Ctx) error {
	after, limit, err := parseExportQuery(c)
	if err != nil {
		return err
	}
	list, cursor, hasMore, err := h.store.Export(c.Context(), after, limit)
	if stderrors.Is(err, ErrCursorAhead) {
		return errors.CursorAheadError()
	}
	if err != nil {
		return dbError(c.Context(), "export audit events", err)
	}
	resp := exportResponse{
		AuditEvents: make([]exportEventResponse, 0, len(list)),
		Cursor:      strconv.FormatInt(cursor, 10),
		HasMore:     hasMore,
	}
	for _, e := range list {
		seq := ""
		if e.FinalizedSeq != nil {
			seq = strconv.FormatInt(*e.FinalizedSeq, 10)
		}
		resp.AuditEvents = append(resp.AuditEvents, exportEventResponse{
			Seq:           seq,
			eventResponse: toResponse(e),
		})
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(resp)
}

func parseExportQuery(c fiber.Ctx) (int64, int, error) {
	var after int64
	limit := defaultExportLimit
	var bad fieldErrs
	seen := map[string]bool{}

	for k, v := range c.Request().URI().QueryArgs().All() {
		key, value := string(k), strings.TrimSpace(string(v))
		if seen[key] {
			bad.add(key)
			continue
		}
		seen[key] = true
		switch key {
		case "after":
			n, ok := parseSeq(value)
			if !ok {
				bad.add(key)
				continue
			}
			after = n
		case "limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				bad.add(key)
				continue
			}
			limit = min(n, maxExportLimit)
		default:
			bad.add(key)
		}
	}
	if err := bad.err(); err != nil {
		return 0, 0, err
	}
	return after, limit, nil
}

// parseSeq accepts a positive decimal integer with no leading zero.
func parseSeq(v string) (int64, bool) {
	if v == "" || v[0] < '1' || v[0] > '9' {
		return 0, false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

func dbError(ctx context.Context, msg string, err error) error {
	httpx.LogError(ctx, msg, err, errors.KindDB)
	if dbx.Unavailable(err) {
		return errors.ServiceUnavailableError()
	}
	return errors.InternalError()
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
			if !validParamName(name) || value == "" || len(value) > maxTextLen || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
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
			n64, err := strconv.ParseInt(value, 10, 32)
			if err != nil {
				bad.add(key)
				continue
			}
			n := int(n64)
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

// filterText is an exact-match value: 1..maxTextLen bytes of visible ASCII (0x21..0x7e).
func filterText(bad *fieldErrs, key, value string) string {
	if value == "" || len(value) > maxTextLen || !visibleASCII(value) {
		bad.add(key)
		return ""
	}
	return value
}

func visibleASCII(s string) bool {
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
