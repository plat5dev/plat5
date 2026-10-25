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

func (f *fakeStore) CreateIntent(_ context.Context, in *Intent, final *Outcome) (bool, error) {
	if f.byRequest == nil {
		f.byRequest = map[string]*Event{}
	}
	if _, ok := f.byRequest[in.RequestID]; ok {
		return false, nil
	}
	e := &Event{Intent: *in, Outcome: Outcome{Outcome: OutcomePending}}
	if final != nil {
		e.Outcome = *final
	}
	f.byRequest[in.RequestID] = e
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
	h := &Handler{store: s}
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

func TestRejectedIsWrittenFinal(t *testing.T) {
	s := &fakeStore{}
	app := newTestApp(s)

	rejected := strings.Replace(validIntent, `"ip": "203.0.113.7",`, `"ip": "203.0.113.7", "outcome": "rejected", "status": 403,`, 1)
	if code, body := do(t, app, http.MethodPut, "/internal/events/req-r", rejected); code != http.StatusCreated {
		t.Fatalf("rejected: %d %s", code, body)
	}
	if got := s.byRequest["req-r"].Outcome; got.Outcome != OutcomeRejected || *got.Status != 403 {
		t.Fatalf("written final: %+v", got)
	}
	if code, _ := do(t, app, http.MethodPut, "/internal/events/req-r", rejected); code != http.StatusOK {
		t.Fatalf("a retried refusal is 200: %d", code)
	}
}

func TestListShape(t *testing.T) {
	s := &fakeStore{list: []*Event{{
		ID: "01ja2z6q3y8d5v2k9n4r7t1w0x",
		Intent: Intent{
			RequestID:  "req-1",
			OccurredAt: time.Date(2026, 10, 8, 18, 30, 0, 120_000_000, time.UTC),
			RootID:     strPtr("01o"),
			Nodes:      []Node{{Kind: "organization", ID: "01o"}},
			Actor: Actor{MemberID: strPtr("01m"), Principal: strPtr(PrincipalServiceAccount), AuthType: strPtr(AuthAPIKey),
				KeyID: strPtr("01k")},
			Service: "identity",
			Method:  strPtr("DELETE"),
			Route:   strPtr("/orgs/{organization}"),
			IP:      strPtr("203.0.113.7"),
		},
		Outcome: Outcome{Outcome: OutcomeRejected, Status: intPtr(403)},
	}}}
	app := newTestApp(s)

	code, body := do(t, app, http.MethodGet, "/nodes/01o/audit-events", "")
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
	for _, k := range []string{"details", "user_agent", "created_id", "named_member_id"} {
		if v, ok := e[k]; !ok || v != nil {
			t.Fatalf("%s is present and null: %s", k, body)
		}
	}
	if n, ok := e["named_nodes"].([]any); !ok || len(n) != 0 {
		t.Fatalf("named_nodes is [] when none are named: %s", body)
	}
	if p, ok := e["params"].(map[string]any); !ok || len(p) != 0 {
		t.Fatalf("params is {} when the route has none: %s", body)
	}
	if e["root_id"] != "01o" || len(e["nodes"].([]any)) != 1 {
		t.Fatalf("root and chain: %s", body)
	}
	actor := e["actor"].(map[string]any)
	if actor["member_id"] != "01m" || actor["user_id"] != nil || actor["principal"] != "service_account" ||
		actor["auth_type"] != "apikey" || actor["key_id"] != "01k" {
		t.Fatalf("actor: %s", body)
	}
	if _, ok := actor["key_prefix"]; ok {
		t.Fatalf("the key is its id: %s", body)
	}
	if s.lastList.Log != "01o" || !s.lastList.NodeRead || s.lastList.Limit != 50 {
		t.Fatalf("filter: %+v", s.lastList)
	}

	// A person's own log: a user route's event has no root and empty nodes.
	s.list = []*Event{{ID: "01ja2z6q3y8d5v2k9n4r7t1w0x", Intent: Intent{
		RequestID: "req-2", OccurredAt: testNow, Nodes: []Node{},
		Actor:   Actor{UserID: strPtr("01u"), Principal: strPtr(PrincipalPerson), AuthType: strPtr(AuthJWT)},
		Service: "identity", Method: strPtr("POST"), Route: strPtr("/orgs"), IP: strPtr("203.0.113.7"),
	}, Outcome: Outcome{Outcome: OutcomeResponded, Status: intPtr(201), NamedNodes: []string{"01o"}, NamedMemberID: strPtr("01m")}}}
	code, body = do(t, app, http.MethodGet, "/users/01u/audit-events", "")
	if code != http.StatusOK {
		t.Fatalf("person's list: %d %s", code, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	e = resp["audit_events"].([]any)[0].(map[string]any)
	if e["root_id"] != nil || len(e["nodes"].([]any)) != 0 || e["named_nodes"].([]any)[0] != "01o" || e["named_member_id"] != "01m" ||
		e["actor"].(map[string]any)["user_id"] != "01u" || e["actor"].(map[string]any)["key_id"] != nil {
		t.Fatalf("user route event: %s", body)
	}
	if s.lastList.Log != "user:01u" || s.lastList.NodeRead {
		t.Fatalf("filter: %+v", s.lastList)
	}
}

func TestListFilters(t *testing.T) {
	s := &fakeStore{}
	app := newTestApp(s)

	q := "?limit=500&starting_after=01ja2z6q3y8d5v2k9n4r7t1w0x&actor_member_id=01m&service=identity&method=PATCH" +
		"&route=%2Forgs%2F%7Borganization%7D%2Fmembers%2F%7Bmember_id%7D&outcome=pending&status=403&request_id=req-1" +
		"&params%5Bmember_id%5D=01n&occurred_after=2026-10-01T00:00:00Z&occurred_before=2026-10-09T00:00:00Z"
	if code, body := do(t, app, http.MethodGet, "/nodes/01o/audit-events"+q, ""); code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	f := s.lastList
	if f.Limit != 100 {
		t.Fatalf("limit is clamped to 100: %d", f.Limit)
	}
	if f.Route != "/orgs/{organization}/members/{member_id}" || f.Params["member_id"] != "01n" || f.ActorMemberID != "01m" ||
		f.Method != "PATCH" || f.Outcome != OutcomePending || f.Status == nil || *f.Status != 403 || f.RequestID != "req-1" ||
		f.OccurredAfter == nil || f.OccurredBefore == nil {
		t.Fatalf("filter: %+v", f)
	}

	for name, q := range map[string]string{
		"unknown param":      "?actor=01m",
		"repeated param":     "?service=a&service=b",
		"bad status":         "?status=4xx",
		"bad cursor":         "?starting_after=nope",
		"uppercase cursor":   "?starting_after=01JA2Z6Q3Y8D5V2K9N4R7T1W0X",
		"bad time":           "?occurred_after=yesterday",
		"empty param":        "?params%5Bmember_id%5D=",
		"bad param name":     "?params%5Bmember-id%5D=m",
		"zero limit":         "?limit=0",
		"empty value":        "?service=",
		"bad request id":     "?request_id=a%20b",
		"bracketless key":    "?params=m",
		"key prefix, no key": "?key_prefix=plat5-k-2",
	} {
		t.Run(name, func(t *testing.T) {
			if code, body := do(t, app, http.MethodGet, "/nodes/01o/audit-events"+q, ""); code != http.StatusUnprocessableEntity {
				t.Fatalf("want 422, got %d %s", code, body)
			}
		})
	}
}
