package lifecycle

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	apierrors "github.com/plat5dev/plat5/identity/errors"
)

type fakeStore struct {
	tree      []*TreeEvent
	users     []*UserEvent
	lastLimit int
	lastAfter int64
}

func after[T any](events []*T, id func(*T) int64, limit int, startingAfter int64) ([]*T, bool) {
	var out []*T
	for _, e := range events {
		if id(e) > startingAfter {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		return out[:limit], true
	}
	return out, false
}

func (f *fakeStore) ListTree(_ context.Context, limit int, startingAfter int64) ([]*TreeEvent, bool, error) {
	f.lastLimit, f.lastAfter = limit, startingAfter
	out, more := after(f.tree, func(e *TreeEvent) int64 { return e.ID }, limit, startingAfter)
	return out, more, nil
}

func (f *fakeStore) ListUsers(_ context.Context, limit int, startingAfter int64) ([]*UserEvent, bool, error) {
	f.lastLimit, f.lastAfter = limit, startingAfter
	out, more := after(f.users, func(e *UserEvent) int64 { return e.ID }, limit, startingAfter)
	return out, more, nil
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

// fields is an event's keys, sorted.
func fields(e map[string]any) []string {
	return slices.Sorted(maps.Keys(e))
}

func TestTreeEventsShape(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{tree: []*TreeEvent{
		{ID: 1, Type: TypeNodeDeleted, Kind: strp("project"), NodeID: strp("01jbx0000000000000000000n1"), RootID: "01jbx0000000000000000000n0", OccurredAt: at},
		{ID: 2, Type: TypeMemberRemoved, RootID: "01jbx0000000000000000000n2", MemberID: strp("01jbx0000000000000000000m1"), Principal: strp("service_account"), OccurredAt: at},
	}}
	status, body := get(t, testApp(store), "/tree-events")
	if status != 200 {
		t.Fatalf("status %d: %v", status, body)
	}
	if body["has_more"] != false || !slices.Equal(fields(body), []string{"has_more", "tree_events"}) {
		t.Fatalf("body: %v", body)
	}
	events := body["tree_events"].([]any)
	if len(events) != 2 {
		t.Fatalf("events: %v", events)
	}
	node := events[0].(map[string]any)
	if node["id"] != "1" || node["type"] != "node.deleted" || node["kind"] != "project" || node["node_id"] != "01jbx0000000000000000000n1" || node["root_id"] != "01jbx0000000000000000000n0" || node["occurred_at"] != "2026-10-13T12:00:00Z" {
		t.Fatalf("node event: %v", node)
	}
	if got := fields(node); !slices.Equal(got, []string{"id", "kind", "node_id", "occurred_at", "root_id", "type"}) {
		t.Fatalf("node event fields: %v", got)
	}
	mem := events[1].(map[string]any)
	if mem["id"] != "2" || mem["type"] != "member.removed" || mem["member_id"] != "01jbx0000000000000000000m1" || mem["root_id"] != "01jbx0000000000000000000n2" || mem["principal"] != "service_account" {
		t.Fatalf("member event: %v", mem)
	}
	if got := fields(mem); !slices.Equal(got, []string{"id", "member_id", "occurred_at", "principal", "root_id", "type"}) {
		t.Fatalf("member event fields: %v", got)
	}
	if store.lastAfter != 0 || store.lastLimit != 50 {
		t.Fatalf("defaults: after %d limit %d", store.lastAfter, store.lastLimit)
	}
}

// A user event is its id, type, user id and time: no tree, no member, no name.
func TestUserEventsShape(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{users: []*UserEvent{
		{ID: 1, Type: TypeUserCreated, UserID: "01jbx0000000000000000000u1", OccurredAt: at},
		{ID: 2, Type: TypeUserDeleted, UserID: "01jbx0000000000000000000u1", OccurredAt: at.Add(time.Hour)},
	}}
	status, body := get(t, testApp(store), "/user-events")
	if status != 200 {
		t.Fatalf("status %d: %v", status, body)
	}
	if body["has_more"] != false || !slices.Equal(fields(body), []string{"has_more", "user_events"}) {
		t.Fatalf("body: %v", body)
	}
	events := body["user_events"].([]any)
	if len(events) != 2 {
		t.Fatalf("events: %v", events)
	}
	for i, want := range []map[string]any{
		{"id": "1", "type": "user.created", "user_id": "01jbx0000000000000000000u1", "occurred_at": "2026-10-13T12:00:00Z"},
		{"id": "2", "type": "user.deleted", "user_id": "01jbx0000000000000000000u1", "occurred_at": "2026-10-13T13:00:00Z"},
	} {
		if got := events[i].(map[string]any); !maps.Equal(got, want) {
			t.Fatalf("event %d: %v, want %v", i, got, want)
		}
	}
	if store.lastAfter != 0 || store.lastLimit != 50 {
		t.Fatalf("defaults: after %d limit %d", store.lastAfter, store.lastLimit)
	}
}

func TestListEmptyIsArray(t *testing.T) {
	for path, key := range map[string]string{"/tree-events": "tree_events", "/user-events": "user_events"} {
		status, body := get(t, testApp(&fakeStore{}), path)
		if status != 200 {
			t.Fatalf("%s: status %d", path, status)
		}
		if events, ok := body[key].([]any); !ok || len(events) != 0 {
			t.Fatalf("%s: want empty array: %v", path, body)
		}
	}
}

func TestListPages(t *testing.T) {
	store := &fakeStore{}
	for i := int64(1); i <= 3; i++ {
		store.tree = append(store.tree, &TreeEvent{ID: i, Type: TypeNodeCreated, Kind: strp("organization"), NodeID: strp("n_1"), RootID: "n_1"})
		store.users = append(store.users, &UserEvent{ID: i, Type: TypeUserCreated, UserID: "u_1"})
	}
	app := testApp(store)
	for path, key := range map[string]string{"/tree-events": "tree_events", "/user-events": "user_events"} {
		_, body := get(t, app, path+"?limit=2")
		if body["has_more"] != true || len(body[key].([]any)) != 2 {
			t.Fatalf("%s page 1: %v", path, body)
		}
		_, body = get(t, app, path+"?limit=2&starting_after=2")
		events := body[key].([]any)
		if body["has_more"] != false || len(events) != 1 || events[0].(map[string]any)["id"] != "3" {
			t.Fatalf("%s page 2: %v", path, body)
		}
		if store.lastAfter != 2 || store.lastLimit != 2 {
			t.Fatalf("%s: starting_after %d limit %d", path, store.lastAfter, store.lastLimit)
		}
		get(t, app, path+"?limit=500")
		if store.lastLimit != 100 {
			t.Fatalf("%s: limit clamped to %d", path, store.lastLimit)
		}
	}
}

func TestListRejectsBadCursor(t *testing.T) {
	app := testApp(&fakeStore{})
	for _, path := range []string{"/tree-events", "/user-events"} {
		for _, v := range []string{"abc", "0", "-1", "01", "1.5", "01J9ZQ8V3X0000000000000000", "99999999999999999999"} {
			status, body := get(t, app, path+"?starting_after="+v)
			if status != 422 {
				t.Fatalf("%s starting_after=%s: status %d %v", path, v, status, body)
			}
		}
	}
}

func TestListRejectsBadLimit(t *testing.T) {
	for _, path := range []string{"/tree-events", "/user-events"} {
		for _, v := range []string{"0", "-1", "x"} {
			if status, _ := get(t, testApp(&fakeStore{}), path+"?limit="+v); status != 422 {
				t.Fatalf("%s limit=%s: status %d", path, v, status)
			}
		}
	}
}

// The old path is gone.
func TestLifecycleEventsPathIsGone(t *testing.T) {
	resp, err := testApp(&fakeStore{}).Test(httptest.NewRequest("GET", "/lifecycle-events", nil))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
