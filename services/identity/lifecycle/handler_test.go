package lifecycle

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"

	apierrors "github.com/plat5dev/plat5/identity/errors"
)

// fakeStore is the log in memory.
type fakeStore struct {
	mu        sync.Mutex
	events    []*Event
	lastLimit int
	lastAfter int64
	lastTypes []string
}

func (f *fakeStore) add(e *Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fakeStore) Head(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		return 0, nil
	}
	return f.events[len(f.events)-1].ID, nil
}

func (f *fakeStore) List(_ context.Context, after int64, limit int, types []string) ([]*Event, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastLimit, f.lastAfter, f.lastTypes = limit, after, types
	var out []*Event
	for _, e := range f.events {
		if e.ID > after && (types == nil || slices.Contains(types, e.Type)) {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

func newTestHandler(store eventStore, wait time.Duration) *Handler {
	return &Handler{store: store, head: NewHead(), wait: wait}
}

func testApp(h *Handler) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	app.Get(Path, h.List)
	return app
}

// following serves store with a short wait, its head followed every 5ms.
func following(t *testing.T, store eventStore, wait time.Duration) *fiber.App {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := newTestHandler(store, wait)
	go h.Follow(ctx, 5*time.Millisecond, zerolog.Nop())
	return testApp(h)
}

func do(t *testing.T, app *fiber.App, path string) (int, []byte) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil), fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func getWait(t *testing.T, app *fiber.App, path string) (int, map[string]any) {
	t.Helper()
	status, raw := do(t, app, path)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("%s: %s: %v", path, raw, err)
	}
	return status, body
}

func get(t *testing.T, app *fiber.App, query string) (int, map[string]any) {
	t.Helper()
	return getWait(t, app, Path+query)
}

func strp(s string) *string { return &s }

// fields is an event's keys, sorted.
func fields(e map[string]any) []string {
	return slices.Sorted(maps.Keys(e))
}

func idsOf(body map[string]any) []string {
	var out []string
	for _, e := range body["events"].([]any) {
		out = append(out, e.(map[string]any)["id"].(string))
	}
	return out
}

func sample() *fakeStore {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	return &fakeStore{events: []*Event{
		{ID: 1, Type: TypeNodeDeleted, Kind: strp("project"), NodeID: strp("01jbx0000000000000000000n1"), RootID: strp("01jbx0000000000000000000n0"), OccurredAt: at},
		{ID: 2, Type: TypeMemberRemoved, RootID: strp("01jbx0000000000000000000n2"), MemberID: strp("01jbx0000000000000000000m1"), Principal: strp("service_account"), OccurredAt: at},
		{ID: 4, Type: TypeUserDeleted, UserID: strp("01jbx0000000000000000000u1"), OccurredAt: at.Add(time.Hour)},
	}}
}

// Each event carries its type's fields and no others, in id order.
func TestEventsShape(t *testing.T) {
	store := sample()
	status, body := get(t, testApp(newTestHandler(store, time.Second)), "")
	if status != 200 {
		t.Fatalf("status %d: %v", status, body)
	}
	if body["has_more"] != false || !slices.Equal(fields(body), []string{"events", "has_more"}) {
		t.Fatalf("body: %v", body)
	}
	events := body["events"].([]any)
	for i, want := range []map[string]any{
		{"id": "1", "type": "node.deleted", "kind": "project", "node_id": "01jbx0000000000000000000n1", "root_id": "01jbx0000000000000000000n0", "occurred_at": "2026-10-13T12:00:00Z"},
		{"id": "2", "type": "member.removed", "member_id": "01jbx0000000000000000000m1", "root_id": "01jbx0000000000000000000n2", "principal": "service_account", "occurred_at": "2026-10-13T12:00:00Z"},
		{"id": "4", "type": "user.deleted", "user_id": "01jbx0000000000000000000u1", "occurred_at": "2026-10-13T13:00:00Z"},
	} {
		if got := events[i].(map[string]any); !maps.Equal(got, want) {
			t.Fatalf("event %d: %v, want %v", i, got, want)
		}
	}
	if store.lastAfter != 0 || store.lastLimit != 50 || store.lastTypes != nil {
		t.Fatalf("defaults: after %d limit %d types %v", store.lastAfter, store.lastLimit, store.lastTypes)
	}
}

// Without after, an empty log answers at once, with an empty array.
func TestEmptyIsArray(t *testing.T) {
	status, body := get(t, testApp(newTestHandler(&fakeStore{}, time.Hour)), "")
	if events, ok := body["events"].([]any); status != 200 || !ok || len(events) != 0 || body["has_more"] != false {
		t.Fatalf("status %d: %v", status, body)
	}
}

func TestPagesAndLimit(t *testing.T) {
	store := &fakeStore{}
	for i := int64(1); i <= 3; i++ {
		store.add(&Event{ID: i, Type: TypeNodeCreated, Kind: strp("organization"), NodeID: strp("n_1"), RootID: strp("n_1")})
	}
	app := testApp(newTestHandler(store, time.Second))
	_, body := get(t, app, "?limit=2")
	if body["has_more"] != true || !slices.Equal(idsOf(body), []string{"1", "2"}) {
		t.Fatalf("page 1: %v", body)
	}
	_, body = get(t, app, "?limit=2&after=2")
	if body["has_more"] != false || !slices.Equal(idsOf(body), []string{"3"}) {
		t.Fatalf("page 2: %v", body)
	}
	if store.lastAfter != 2 || store.lastLimit != 2 {
		t.Fatalf("after %d limit %d", store.lastAfter, store.lastLimit)
	}
	get(t, app, "?limit=500")
	if store.lastLimit != 100 {
		t.Fatalf("limit clamped to %d", store.lastLimit)
	}
	get(t, app, "?limit=100")
	if store.lastLimit != 100 {
		t.Fatalf("limit %d", store.lastLimit)
	}
}

func TestTypes(t *testing.T) {
	store := sample()
	app := testApp(newTestHandler(store, 50*time.Millisecond))
	_, body := get(t, app, "?types=user.deleted,node.deleted")
	if !slices.Equal(idsOf(body), []string{"1", "4"}) || !slices.Equal(store.lastTypes, []string{"user.deleted", "node.deleted"}) {
		t.Fatalf("types: %v %v", body, store.lastTypes)
	}
	_, body = get(t, app, "?types=node.deleted,member.removed&limit=1")
	if !slices.Equal(idsOf(body), []string{"1"}) || body["has_more"] != true {
		t.Fatalf("has_more counts only those types: %v", body)
	}
	// None left of those types: a wait, then 204.
	if status, raw := do(t, app, Path+"?types=node.deleted,member.removed&after=2"); status != 204 {
		t.Fatalf("none left of those types: %d %s", status, raw)
	}
	for _, typ := range Types {
		if status, _ := get(t, app, "?types="+typ); status != 200 {
			t.Fatalf("%s: %d", typ, status)
		}
	}
}

// Every 422 is VALIDATION_ERROR, naming the param.
func TestValidation(t *testing.T) {
	app := testApp(newTestHandler(&fakeStore{}, time.Hour))
	cases := map[string][]string{
		"after": {"", "abc", "0", "-1", "01", "+1", "1.5", " 1", "1e3", "01J9ZQ8V3X0000000000000000", "99999999999999999999"},
		"limit": {"0", "-1", "x", "1.5"},
		"types": {"", "node.moved", "node.purged,", ",node.purged", "node.purged,,user.deleted", "Node.Purged", "node.*", " node.purged"},
	}
	for param, values := range cases {
		for _, v := range values {
			status, body := get(t, app, "?"+param+"="+urlEscape(v))
			apiErr, _ := body["error"].(map[string]any)
			if status != 422 || apiErr["code"] != "VALIDATION_ERROR" {
				t.Fatalf("%s=%q: status %d %v", param, v, status, body)
			}
			details, _ := apiErr["details"].(map[string]any)
			flds, _ := details["fields"].([]any)
			if len(flds) != 1 || flds[0].(map[string]any)["path"] != param {
				t.Fatalf("%s=%q: names %v", param, v, body)
			}
		}
	}
}

func urlEscape(v string) string {
	out := ""
	for _, r := range v {
		switch r {
		case ' ':
			out += "%20"
		case '+':
			out += "%2B"
		default:
			out += string(r)
		}
	}
	return out
}

// An after past the head (identity's database was reset) answers at once.
func TestAfterPastHead(t *testing.T) {
	app := testApp(newTestHandler(sample(), time.Hour))
	start := time.Now()
	status, body := get(t, app, "?after=5")
	if status != 200 || len(body["events"].([]any)) != 0 || body["has_more"] != false || time.Since(start) > time.Second {
		t.Fatalf("status %d after %v: %v", status, time.Since(start), body)
	}
	if status, _ := get(t, testApp(newTestHandler(&fakeStore{}, time.Hour)), "?after=1"); status != 200 {
		t.Fatalf("empty log: %d", status)
	}
}

// With after and nothing newer, the read waits, then answers 204.
func TestWaitEndsIn204(t *testing.T) {
	app := following(t, sample(), 100*time.Millisecond)
	start := time.Now()
	status, raw := do(t, app, Path+"?after=4")
	if status != 204 || len(raw) != 0 {
		t.Fatalf("status %d: %s", status, raw)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("waited %v", waited)
	}
}

// A waiting read returns when an event lands, without waiting out the wait.
func TestWaitReturnsOnEvent(t *testing.T) {
	store := sample()
	app := following(t, store, 10*time.Second)
	go func() {
		time.Sleep(50 * time.Millisecond)
		store.add(&Event{ID: 5, Type: TypeNodePurged, Kind: strp("project"), NodeID: strp("n_5"), RootID: strp("n_0")})
	}()
	start := time.Now()
	status, body := get(t, app, "?after=4")
	if status != 200 || !slices.Equal(idsOf(body), []string{"5"}) || time.Since(start) > 5*time.Second {
		t.Fatalf("status %d after %v: %v", status, time.Since(start), body)
	}
}

// Only events of types end a wait: one of another type keeps it waiting.
func TestTypesFilterWaits(t *testing.T) {
	store := sample()
	app := following(t, store, 10*time.Second)
	go func() {
		time.Sleep(50 * time.Millisecond)
		store.add(&Event{ID: 5, Type: TypeNodeCreated, Kind: strp("project"), NodeID: strp("n_5"), RootID: strp("n_0")})
		time.Sleep(100 * time.Millisecond)
		store.add(&Event{ID: 6, Type: TypeUserDeleted, UserID: strp("u_2")})
	}()
	start := time.Now()
	status, body := get(t, app, "?after=4&types=user.deleted")
	if status != 200 || !slices.Equal(idsOf(body), []string{"6"}) {
		t.Fatalf("status %d: %v", status, body)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond || waited > 5*time.Second {
		t.Fatalf("waited %v", waited)
	}

	// Nothing of types in the wait: 204, though the log moved.
	app = following(t, store, 100*time.Millisecond)
	go func() {
		time.Sleep(20 * time.Millisecond)
		store.add(&Event{ID: 7, Type: TypeNodeCreated, Kind: strp("project"), NodeID: strp("n_7"), RootID: strp("n_0")})
	}()
	if status, raw := do(t, app, Path+"?after=6&types=user.deleted,node.purged"); status != 204 {
		t.Fatalf("status %d: %s", status, raw)
	}
}

// When Follow stops (shutdown), waiting reads end at once with 204.
func TestShutdownEndsWaits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := newTestHandler(sample(), time.Hour)
	go h.Follow(ctx, 5*time.Millisecond, zerolog.Nop())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if status, _ := do(t, testApp(h), Path+"?after=4"); status != 204 {
		t.Fatalf("status %d", status)
	}
}
