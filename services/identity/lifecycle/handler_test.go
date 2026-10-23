package lifecycle

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	apierrors "github.com/plat5dev/plat5/identity/errors"
)

type fakeStore struct {
	events    []*Event
	lastLimit int
	lastAfter int64
}

func (f *fakeStore) List(_ context.Context, limit int, startingAfter int64) ([]*Event, bool, error) {
	f.lastLimit, f.lastAfter = limit, startingAfter
	var out []*Event
	for _, e := range f.events {
		if e.ID > startingAfter {
			out = append(out, e)
		}
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

func testApp(store eventStore) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	(&Handler{store: store}).MountInternal(app)
	return app
}

func get(t *testing.T, app *fiber.App, path string) (int, map[string]any) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return resp.StatusCode, body
}

func strp(s string) *string { return &s }

func TestListShapesEvents(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{events: []*Event{
		{ID: 1, Type: TypeNodeDeleted, Kind: strp("project"), NodeID: strp("n_1"), RootID: "n_0", OccurredAt: at},
		{ID: 2, Type: TypeMemberRemoved, RootID: "n_2", MemberID: strp("m_1"), Principal: strp("service_account"), OccurredAt: at},
	}}
	status, body := get(t, testApp(store), "/lifecycle-events")
	if status != 200 {
		t.Fatalf("status %d: %v", status, body)
	}
	if body["has_more"] != false {
		t.Fatalf("has_more: %v", body["has_more"])
	}
	events := body["lifecycle_events"].([]any)
	if len(events) != 2 {
		t.Fatalf("events: %v", events)
	}
	node := events[0].(map[string]any)
	if node["id"] != "1" || node["type"] != "project.deleted" || node["node_id"] != "n_1" || node["root_id"] != "n_0" || node["occurred_at"] != "2026-10-13T12:00:00Z" {
		t.Fatalf("node event: %v", node)
	}
	for _, field := range []string{"member_id", "principal", "kind"} {
		if _, ok := node[field]; ok {
			t.Fatalf("node event must not carry %s: %v", field, node)
		}
	}
	mem := events[1].(map[string]any)
	if mem["id"] != "2" || mem["type"] != "member.removed" || mem["member_id"] != "m_1" || mem["root_id"] != "n_2" || mem["principal"] != "service_account" {
		t.Fatalf("member event: %v", mem)
	}
	if _, ok := mem["node_id"]; ok {
		t.Fatalf("member event must not carry node_id: %v", mem)
	}
	if store.lastAfter != 0 || store.lastLimit != 50 {
		t.Fatalf("defaults: after %d limit %d", store.lastAfter, store.lastLimit)
	}
}

func TestListEmptyIsArray(t *testing.T) {
	status, body := get(t, testApp(&fakeStore{}), "/lifecycle-events")
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	if events, ok := body["lifecycle_events"].([]any); !ok || len(events) != 0 {
		t.Fatalf("want empty array: %v", body)
	}
}

func TestListPages(t *testing.T) {
	store := &fakeStore{}
	for i := int64(1); i <= 3; i++ {
		store.events = append(store.events, &Event{ID: i, Type: TypeNodeCreated, Kind: strp("organization"), NodeID: strp("n_1"), RootID: "n_1"})
	}
	app := testApp(store)
	_, body := get(t, app, "/lifecycle-events?limit=2")
	if body["has_more"] != true || len(body["lifecycle_events"].([]any)) != 2 {
		t.Fatalf("page 1: %v", body)
	}
	_, body = get(t, app, "/lifecycle-events?limit=2&starting_after=2")
	events := body["lifecycle_events"].([]any)
	if body["has_more"] != false || len(events) != 1 || events[0].(map[string]any)["id"] != "3" {
		t.Fatalf("page 2: %v", body)
	}
	if store.lastAfter != 2 {
		t.Fatalf("starting_after: %d", store.lastAfter)
	}
}

func TestListRejectsBadCursor(t *testing.T) {
	app := testApp(&fakeStore{})
	for _, v := range []string{"abc", "0", "-1", "01", "1.5", "01J9ZQ8V3X0000000000000000", "99999999999999999999"} {
		status, body := get(t, app, "/lifecycle-events?starting_after="+v)
		if status != 422 {
			t.Fatalf("starting_after=%s: status %d %v", v, status, body)
		}
	}
}

func TestListRejectsBadLimit(t *testing.T) {
	status, _ := get(t, testApp(&fakeStore{}), "/lifecycle-events?limit=0")
	if status != 422 {
		t.Fatalf("status %d", status)
	}
}
