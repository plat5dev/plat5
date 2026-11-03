package lifecycle

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/plat5dev/plat5/identity/internal/testdb"
)

const (
	n1 = "01jbx0000000000000000000n1"
	n2 = "01jbx0000000000000000000n2"
	m1 = "01jbx0000000000000000000m1"
	u1 = "01jbx0000000000000000000u1"
	u2 = "01jbx0000000000000000000u2"
)

func write(pool *pgxpool.Pool, events ...Event) error {
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, e := range events {
		if err := Record(ctx, tx, e); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func record(t *testing.T, pool *pgxpool.Pool, events ...Event) {
	t.Helper()
	if err := write(pool, events...); err != nil {
		t.Fatal(err)
	}
}

func ids(events []*Event) []int64 {
	out := make([]int64, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}

func TestRecordAndList(t *testing.T) {
	pool := testdb.Pool(t, "identity_test_lifecycle")
	ctx := context.Background()
	store := NewStore(pool)
	if head, err := store.Head(ctx); err != nil || head != 0 {
		t.Fatalf("empty head: %d %v", head, err)
	}
	record(t, pool,
		NodeEvent(TypeNodeCreated, "organization", n1, n1),
		NodeEvent(TypeNodePurged, "project", n2, n1),
		MemberEvent(TypeMemberRemoved, m1, n1, "person"),
		UserCreated(u1),
		UserDeleted(u2),
	)

	// One sequence, in write order.
	page, more, err := store.List(ctx, 0, 2, nil)
	if err != nil || !slices.Equal(ids(page), []int64{1, 2}) || !more {
		t.Fatalf("page 1: %v %v %v", ids(page), more, err)
	}
	if p := page[1]; p.Type != TypeNodePurged || *p.Kind != "project" || *p.NodeID != n2 || *p.RootID != n1 || p.MemberID != nil || p.UserID != nil {
		t.Fatalf("node event: %+v", p)
	}
	rest, more, err := store.List(ctx, 2, 50, nil)
	if err != nil || !slices.Equal(ids(rest), []int64{3, 4, 5}) || more {
		t.Fatalf("page 2: %v %v %v", ids(rest), more, err)
	}
	if m := rest[0]; m.Type != TypeMemberRemoved || m.Kind != nil || m.NodeID != nil || *m.MemberID != m1 || *m.Principal != "person" || *m.RootID != n1 || m.UserID != nil {
		t.Fatalf("member event: %+v", m)
	}
	if u := rest[2]; u.Type != TypeUserDeleted || *u.UserID != u2 || u.RootID != nil || u.MemberID != nil || u.NodeID != nil {
		t.Fatalf("user event: %+v", u)
	}
	// One transaction's events share occurred_at.
	if !page[0].OccurredAt.Equal(rest[2].OccurredAt) {
		t.Fatalf("occurred_at: %v %v", page[0].OccurredAt, rest[2].OccurredAt)
	}
	if head, err := store.Head(ctx); err != nil || head != 5 {
		t.Fatalf("head: %d %v", head, err)
	}
	if past, more, err := store.List(ctx, 5, 50, nil); err != nil || len(past) != 0 || more {
		t.Fatalf("past the end: %v %v %v", past, more, err)
	}

	// types: only those, and has_more counts only those.
	duties := []string{TypeNodePurged, TypeUserDeleted}
	got, more, err := store.List(ctx, 0, 1, duties)
	if err != nil || !slices.Equal(ids(got), []int64{2}) || !more {
		t.Fatalf("types page 1: %v %v %v", ids(got), more, err)
	}
	got, more, err = store.List(ctx, 2, 1, duties)
	if err != nil || !slices.Equal(ids(got), []int64{5}) || more {
		t.Fatalf("types page 2: %v %v %v", ids(got), more, err)
	}
	if got, _, _ := store.List(ctx, 0, 50, []string{TypeMemberCreated}); len(got) != 0 {
		t.Fatalf("no member.created: %v", ids(got))
	}
}

// Each type carries its own fields and no others, and only the eight types
// exist.
func TestEventChecks(t *testing.T) {
	pool := testdb.Pool(t, "identity_test_lifecycle_checks")
	ctx := context.Background()
	s := func(v string) *string { return &v }
	bad := map[string]Event{
		"unknown type":             {Type: "node.moved", Kind: s("project"), NodeID: s(n1), RootID: s(n1)},
		"node without kind":        {Type: TypeNodeCreated, NodeID: s(n1), RootID: s(n1)},
		"node without node_id":     {Type: TypeNodeCreated, Kind: s("project"), RootID: s(n1)},
		"node without root_id":     {Type: TypeNodeCreated, Kind: s("project"), NodeID: s(n1)},
		"node with a member":       {Type: TypeNodeCreated, Kind: s("project"), NodeID: s(n1), RootID: s(n1), MemberID: s(m1)},
		"node with a principal":    {Type: TypeNodeCreated, Kind: s("project"), NodeID: s(n1), RootID: s(n1), Principal: s("person")},
		"node with a user id":      {Type: TypeNodeCreated, Kind: s("project"), NodeID: s(n1), RootID: s(n1), UserID: s(u1)},
		"member without member":    {Type: TypeMemberCreated, RootID: s(n1), Principal: s("person")},
		"member without principal": {Type: TypeMemberCreated, MemberID: s(m1), RootID: s(n1)},
		"member without root_id":   {Type: TypeMemberCreated, MemberID: s(m1), Principal: s("person")},
		"member with a kind":       {Type: TypeMemberCreated, MemberID: s(m1), RootID: s(n1), Principal: s("person"), Kind: s("project")},
		"member with a node_id":    {Type: TypeMemberCreated, MemberID: s(m1), RootID: s(n1), Principal: s("person"), NodeID: s(n1)},
		"member with a user id":    {Type: TypeMemberRemoved, MemberID: s(m1), RootID: s(n1), Principal: s("person"), UserID: s(u1)},
		"bad principal":            {Type: TypeMemberCreated, MemberID: s(m1), RootID: s(n1), Principal: s("robot")},
		"user without user id":     {Type: TypeUserCreated},
		"user with a root":         {Type: TypeUserDeleted, UserID: s(u1), RootID: s(n1)},
		"user with a member":       {Type: TypeUserDeleted, UserID: s(u1), MemberID: s(m1)},
		"user with a node":         {Type: TypeUserDeleted, UserID: s(u1), NodeID: s(n1)},
		"user with a kind":         {Type: TypeUserDeleted, UserID: s(u1), Kind: s("project")},
		"user with a principal":    {Type: TypeUserDeleted, UserID: s(u1), Principal: s("person")},
	}
	for name, e := range bad {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = Record(ctx, tx, e)
		_ = tx.Rollback(ctx)
		if err == nil {
			t.Errorf("%s: recorded", name)
		}
	}
	// The constructors' events all pass.
	record(t, pool,
		NodeEvent(TypeNodeDeleted, "project", n2, n1),
		MemberEvent(TypeMemberCreated, m1, n1, "service_account"),
		UserCreated(u1),
	)
}

// Many transactions writing at once, each holding its transaction open a
// while after its events, while a reader polls throughout: the reader never
// sees an id before every lower one, so it never walks past an event.
func TestConcurrentWritersLeaveNoGap(t *testing.T) {
	pool := testdb.Pool(t, "identity_test_lifecycle_concurrent")
	ctx := context.Background()
	store := NewStore(pool)
	const writers = 32
	const rounds = 5

	writeOne := func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		// An account deletion's shape: member events, then the user event.
		for range 1 + rand.IntN(3) {
			if err := Record(ctx, tx, MemberEvent(TypeMemberRemoved, m1, n1, "person")); err != nil {
				return err
			}
		}
		if err := Record(ctx, tx, UserDeleted(u1)); err != nil {
			return err
		}
		time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
		return tx.Commit(ctx)
	}

	done := make(chan struct{})
	var seen []*Event
	readerErr := make(chan error, 1)
	go func() {
		var cursor int64
		for {
			// Sampled before the read, so the last read follows every commit.
			finished := false
			select {
			case <-done:
				finished = true
			default:
			}
			page, _, err := store.List(ctx, cursor, 100, nil)
			if err != nil {
				readerErr <- err
				return
			}
			for _, e := range page {
				if e.ID != cursor+1 {
					readerErr <- fmt.Errorf("reader at %d saw %d first", cursor, e.ID)
					return
				}
				cursor = e.ID
				seen = append(seen, e)
			}
			if finished && len(page) == 0 {
				readerErr <- nil
				return
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, writers*rounds)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				errs <- writeOne()
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(done)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := <-readerErr; err != nil {
		t.Fatal(err)
	}
	head, err := store.Head(ctx)
	if err != nil || head != int64(len(seen)) || len(seen) < writers*rounds*2 {
		t.Fatalf("reader saw %d of head %d (%v)", len(seen), head, err)
	}
	// A transaction's events are adjacent: each run of member.removed ends in
	// its user.deleted, with one occurred_at.
	for i := 0; i < len(seen); {
		j := i
		for seen[j].Type == TypeMemberRemoved {
			j++
		}
		if seen[j].Type != TypeUserDeleted || j == i {
			t.Fatalf("event %d: a transaction's events not adjacent", seen[j].ID)
		}
		for k := i; k <= j; k++ {
			if !seen[k].OccurredAt.Equal(seen[i].OccurredAt) {
				t.Fatalf("event %d: occurred_at differs within a transaction", seen[k].ID)
			}
		}
		i = j + 1
	}
}

// A waiting read against Postgres returns when another transaction's event
// lands, woken by Follow.
func TestWaitWakesOnCommit(t *testing.T) {
	pool := testdb.Pool(t, "identity_test_lifecycle_wait")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	record(t, pool, UserCreated(u1))

	h := NewHandler(NewStore(pool), 10*time.Second)
	go h.Follow(ctx, 10*time.Millisecond, zerolog.Nop())
	app := testApp(h)

	go func() {
		time.Sleep(100 * time.Millisecond)
		if err := write(pool, NodeEvent(TypeNodeCreated, "organization", n1, n1), UserDeleted(u1)); err != nil {
			t.Error(err)
		}
	}()
	start := time.Now()
	status, body := getWait(t, app, "/internal/events?after=1&types=user.deleted")
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	events := body["events"].([]any)
	if len(events) != 1 || events[0].(map[string]any)["id"] != "3" || time.Since(start) > 5*time.Second {
		t.Fatalf("woke with %v after %v", body, time.Since(start))
	}
}
