package lifecycle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/internal/testdb"
)

type fakeConfigRev struct {
	rev     atomic.Int64
	mu      sync.Mutex
	changed chan struct{}
}

func newFakeConfigRev(initial int64) *fakeConfigRev {
	f := &fakeConfigRev{}
	f.rev.Store(initial)
	return f
}

func (f *fakeConfigRev) Revision() int64 { return f.rev.Load() }

func (f *fakeConfigRev) Set(rev int64) {
	f.rev.Store(rev)
	f.mu.Lock()
	if f.changed != nil {
		close(f.changed)
		f.changed = nil
	}
	f.mu.Unlock()
}

func (f *fakeConfigRev) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.changed == nil {
		f.changed = make(chan struct{})
	}
	return f.changed
}

func TestInvalidationsStoreAndWatermark(t *testing.T) {
	pool := testdb.Pool(t, "lifecycle_inv_store")
	ctx := context.Background()
	store := NewInvalidationStore(pool)

	head, pruned, err := store.State(ctx)
	if err != nil || head != 0 || pruned != 0 {
		t.Fatalf("empty state: head=%d pruned=%d err=%v", head, pruned, err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordCommit(ctx, tx, nil, []Invalidation{
		InvalidateKey("01k1"),
		InvalidateMember("01m1"),
		InvalidateRoot("01r1"),
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	head, pruned, err = store.State(ctx)
	if err != nil || head != 3 || pruned != 0 {
		t.Fatalf("after insert: head=%d pruned=%d err=%v", head, pruned, err)
	}
	list, err := store.List(ctx, 1, 10)
	if err != nil || len(list) != 2 || list[0].Scope != ScopeMember || list[1].Scope != ScopeRoot {
		t.Fatalf("list after 1: %+v %v", list, err)
	}

	// Prune everything and verify watermark advances to 3.
	if _, err := pool.Exec(ctx, `UPDATE admit_invalidations SET created_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	removed, err := store.PruneOlderThan(ctx, time.Hour)
	if err != nil || removed != 3 {
		t.Fatalf("prune: removed=%d err=%v", removed, err)
	}
	head, pruned, err = store.State(ctx)
	if err != nil || head != 0 || pruned != 3 {
		t.Fatalf("after prune: head=%d pruned=%d err=%v", head, pruned, err)
	}
}

func TestInvalidationHandlerLongPollAndReset(t *testing.T) {
	pool := testdb.Pool(t, "lifecycle_inv_handler")
	ctx := context.Background()
	store := NewInvalidationStore(pool)
	cfgRev := newFakeConfigRev(5)
	h := NewInvalidationHandler(store, cfgRev, 200*time.Millisecond)

	app := fiber.New()
	app.Get(InvalidationsPath, h.List)

	get := func(path string) *http.Response {
		t.Helper()
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil), fiber.TestConfig{Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// 1. Initial sync (no after) returns immediately with head=0, config_revision=5.
	resp := get(InvalidationsPath)
	if resp.StatusCode != 200 {
		t.Fatalf("initial sync status=%d", resp.StatusCode)
	}
	var body InvalidationResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Head != 0 || body.ConfigRevision != 5 || body.Reset || len(body.Events) != 0 {
		t.Fatalf("initial sync body=%+v", body)
	}

	// 2. Idle poll times out with 204.
	resp = get(InvalidationsPath + "?after=0&config_revision=5")
	if resp.StatusCode != 204 {
		t.Fatalf("idle poll status=%d, want 204", resp.StatusCode)
	}

	// 3. Long poll wakes on config_revision change.
	done := make(chan InvalidationResponse, 1)
	go func() {
		r := get(InvalidationsPath + "?after=0&config_revision=5")
		var out InvalidationResponse
		_ = json.NewDecoder(r.Body).Decode(&out)
		done <- out
	}()
	time.Sleep(25 * time.Millisecond)
	cfgRev.Set(6)
	got := <-done
	if got.ConfigRevision != 6 || got.Reset {
		t.Fatalf("config rev wake: %+v", got)
	}

	// 4. Cursor ahead of head returns reset: true immediately.
	resp = get(InvalidationsPath + "?after=99&config_revision=6")
	if resp.StatusCode != 200 {
		t.Fatalf("reset status=%d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Reset || body.Head != 0 {
		t.Fatalf("expected reset=true: %+v", body)
	}

	// 5. Long poll wakes when a new invalidation commits and Notify is called.
	go func() {
		r := get(InvalidationsPath + "?after=0&config_revision=6")
		var out InvalidationResponse
		_ = json.NewDecoder(r.Body).Decode(&out)
		done <- out
	}()
	time.Sleep(25 * time.Millisecond)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordCommit(ctx, tx, nil, []Invalidation{InvalidateUser("01u1")}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	h.Notify(ctx)
	got = <-done
	if got.Reset || got.Head != 1 || len(got.Events) != 1 || got.Events[0].Scope != ScopeUser || got.Events[0].Target != "01u1" {
		t.Fatalf("event wake: %+v", got)
	}
}
