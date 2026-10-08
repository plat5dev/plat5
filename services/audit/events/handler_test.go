package events

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/audit/errors"
	"github.com/plat5dev/plat5/audit/metrics"
)

type fakeStore struct {
	byRequest map[string]*Event
	lastList  Filter
	list      []*Event
}

func (f *fakeStore) CreateIntent(_ context.Context, in *Intent) (bool, error) {
	if f.byRequest == nil {
		f.byRequest = map[string]*Event{}
	}
	if _, ok := f.byRequest[in.RequestID]; ok {
		return false, nil
	}
	f.byRequest[in.RequestID] = &Event{Intent: *in, Outcome: Outcome{Outcome: OutcomePending}}
	return true, nil
}

func (f *fakeStore) ApplyOutcome(_ context.Context, requestID string, out *Outcome) (string, error) {
	e, ok := f.byRequest[requestID]
	if !ok {
		return "", ErrNotFound
	}
	if e.Outcome.Outcome != OutcomePending {
		return metrics.OutcomeFinal, nil
	}
	e.Outcome = *out
	return metrics.OutcomeApplied, nil
}

func (f *fakeStore) List(_ context.Context, filter Filter) ([]*Event, bool, error) {
	f.lastList = filter
	return f.list, false, nil
}

func newTestApp(s *fakeStore) *fiber.App {
	h := &Handler{store: s, now: func() time.Time { return testNow }}
	app := fiber.New(fiber.Config{ErrorHandler: errors.FiberErrorHandler})
	h.MountPublic(app)
	h.MountInternal(app.Group("/internal"))
	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestIntentThenOutcome(t *testing.T) {
	s := &fakeStore{}
	app := newTestApp(s)

	if code, body := do(t, app, http.MethodPut, "/internal/events/req-1", validIntent); code != http.StatusCreated {
		t.Fatalf("intent: %d %s", code, body)
	}
	if code, _ := do(t, app, http.MethodPut, "/internal/events/req-1", validIntent); code != http.StatusOK {
		t.Fatalf("a retried intent is 200: %d", code)
	}
	if code, body := do(t, app, http.MethodPut, "/internal/events/req-2", `{}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed intent: %d %s", code, body)
	}

	responded := `{"outcome":"responded","status":200,"details":{"role":{"from":"developer","to":"admin"}}}`
	if code, body := do(t, app, http.MethodPatch, "/internal/events/req-1", responded); code != http.StatusNoContent {
		t.Fatalf("outcome: %d %s", code, body)
	}
	if code, _ := do(t, app, http.MethodPatch, "/internal/events/req-1", `{"outcome":"no_response","status":502}`); code != http.StatusNoContent {
		t.Fatalf("an outcome on a final event is 204: %d", code)
	}
	if got := s.byRequest["req-1"].Outcome; got.Outcome != OutcomeResponded || *got.Status != 200 {
		t.Fatalf("the first outcome wins: %+v", got)
	}
	if code, _ := do(t, app, http.MethodPatch, "/internal/events/nope", responded); code != http.StatusNotFound {
		t.Fatalf("an outcome with no intent is 404: %d", code)
	}
}

func TestListShape(t *testing.T) {
	status := 403
	s := &fakeStore{list: []*Event{{
		ID: "01JA2Z6Q3Y8D5V2K9N4R7T1W0X",
		Intent: Intent{
			RequestID:      "req-1",
			OccurredAt:     time.Date(2026, 10, 8, 18, 30, 0, 120_000_000, time.UTC),
			OrganizationID: "org-1",
			Actor:          Actor{MemberID: "m-1", AuthType: AuthMemberAPIKey, KeyPrefix: "plat5-mk-1-AbC9"},
			Service:        "identity",
			Method:         "DELETE",
			Route:          "/org",
			IP:             "203.0.113.7",
		},
		Outcome: Outcome{Outcome: OutcomeRejected, Status: &status},
	}}}
	app := newTestApp(s)

	code, body := do(t, app, http.MethodGet, "/organizations/org-1/audit-events", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp["has_more"] != false {
		t.Fatalf("has_more: %s", body)
	}
	e := resp["audit_events"].([]any)[0].(map[string]any)
	if e["occurred_at"] != "2026-10-08T18:30:00.120Z" {
		t.Fatalf("occurred_at is UTC with milliseconds: %v", e["occurred_at"])
	}
	for _, k := range []string{"details", "user_agent"} {
		if v, ok := e[k]; !ok || v != nil {
			t.Fatalf("%s is present and null: %s", k, body)
		}
	}
	if p, ok := e["params"].(map[string]any); !ok || len(p) != 0 {
		t.Fatalf("params is {} when the route has none: %s", body)
	}
	if s.lastList.OrganizationID != "org-1" || s.lastList.Limit != 50 {
		t.Fatalf("filter: %+v", s.lastList)
	}
}

func TestListFilters(t *testing.T) {
	s := &fakeStore{}
	app := newTestApp(s)

	q := "?limit=500&starting_after=01JA2Z6Q3Y8D5V2K9N4R7T1W0X&actor_member_id=m-1&service=identity&method=PATCH" +
		"&route=%2Forg%2Fmembers%2F%7Bmember_id%7D&outcome=pending&request_id=req-1" +
		"&params%5Bmember_id%5D=m-2&occurred_after=2026-10-01T00:00:00Z&occurred_before=2026-10-09T00:00:00Z"
	if code, body := do(t, app, http.MethodGet, "/organizations/org-1/audit-events"+q, ""); code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	f := s.lastList
	if f.Limit != 100 {
		t.Fatalf("limit is clamped to 100: %d", f.Limit)
	}
	if f.Route != "/org/members/{member_id}" || f.Params["member_id"] != "m-2" || f.ActorMemberID != "m-1" ||
		f.Method != "PATCH" || f.Outcome != OutcomePending || f.OccurredAfter == nil || f.OccurredBefore == nil {
		t.Fatalf("filter: %+v", f)
	}

	for name, q := range map[string]string{
		"unknown param":   "?actor=m-1",
		"repeated param":  "?service=a&service=b",
		"bad outcome":     "?outcome=done",
		"bad cursor":      "?starting_after=nope",
		"bad time":        "?occurred_after=yesterday",
		"empty param":     "?params%5Bmember_id%5D=",
		"bad param name":  "?params%5Bmember-id%5D=m",
		"zero limit":      "?limit=0",
		"unaudited verb":  "?method=OPTIONS",
		"empty value":     "?service=",
		"bad request id":  "?request_id=a%20b",
		"lowercase verb":  "?method=patch",
		"bracketless key": "?params=m",
	} {
		t.Run(name, func(t *testing.T) {
			if code, body := do(t, app, http.MethodGet, "/organizations/org-1/audit-events"+q, ""); code != http.StatusUnprocessableEntity {
				t.Fatalf("want 422, got %d %s", code, body)
			}
		})
	}
}
