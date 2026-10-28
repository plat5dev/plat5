package lifecycle

import (
	"context"
	"testing"

	"github.com/plat5dev/plat5/identity/internal/testdb"
)

func TestRecordAndList(t *testing.T) {
	pool := testdb.Pool(t, "identity_test_lifecycle")
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []TreeEvent{
		NodeEvent(TypeNodeCreated, "organization", "01jbx0000000000000000000n1", "01jbx0000000000000000000n1"),
		NodeEvent(TypeNodePurged, "project", "01jbx0000000000000000000n2", "01jbx0000000000000000000n1"),
		MemberEvent(TypeMemberRemoved, "01jbx0000000000000000000m1", "01jbx0000000000000000000n1", "person"),
	} {
		if err := RecordTree(ctx, tx, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []UserEvent{UserCreated("01jbx0000000000000000000u1"), UserDeleted("01jbx0000000000000000000u2")} {
		if err := RecordUser(ctx, tx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	store := NewStore(pool)
	page, more, err := store.ListTree(ctx, 2, 0)
	if err != nil || len(page) != 2 || !more {
		t.Fatalf("page 1: %v %v %v", page, more, err)
	}
	if page[0].Type != TypeNodeCreated || *page[0].Kind != "organization" || page[1].Type != TypeNodePurged || *page[1].Kind != "project" || *page[1].NodeID != "01jbx0000000000000000000n2" {
		t.Fatalf("node events: %+v %+v", page[0], page[1])
	}
	rest, more, err := store.ListTree(ctx, 2, page[1].ID)
	if err != nil || len(rest) != 1 || more {
		t.Fatalf("page 2: %v %v %v", rest, more, err)
	}
	if m := rest[0]; m.Type != TypeMemberRemoved || m.Kind != nil || *m.MemberID != "01jbx0000000000000000000m1" || *m.Principal != "person" || m.NodeID != nil || m.RootID != "01jbx0000000000000000000n1" {
		t.Fatalf("member event: %+v", m)
	}

	// User events have their own sequence, from 1, whatever tree events hold.
	users, more, err := store.ListUsers(ctx, 50, 0)
	if err != nil || len(users) != 2 || more {
		t.Fatalf("user events: %v %v %v", users, more, err)
	}
	if u := users[0]; u.ID != 1 || u.Type != TypeUserCreated || u.UserID != "01jbx0000000000000000000u1" {
		t.Fatalf("user.created: %+v", u)
	}
	if u := users[1]; u.ID != 2 || u.Type != TypeUserDeleted || u.UserID != "01jbx0000000000000000000u2" {
		t.Fatalf("user.deleted: %+v", u)
	}
	if users, _, _ := store.ListUsers(ctx, 50, 2); len(users) != 0 {
		t.Fatalf("past the end: %v", users)
	}

	// One transaction's events share occurred_at, across both feeds.
	if !users[0].OccurredAt.Equal(page[0].OccurredAt) || !users[1].OccurredAt.Equal(rest[0].OccurredAt) {
		t.Fatalf("occurred_at: tree %v %v, users %v %v", page[0].OccurredAt, rest[0].OccurredAt, users[0].OccurredAt, users[1].OccurredAt)
	}
}

// Writers that each record a tree event then a user event, as an account
// deletion does, commit in id order in both feeds, and never deadlock.
func TestRecordConcurrently(t *testing.T) {
	pool := testdb.Pool(t, "identity_test_lifecycle_concurrent")
	ctx := context.Background()
	const writers = 16
	write := func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err := RecordTree(ctx, tx, MemberEvent(TypeMemberRemoved, "01jbx0000000000000000000m1", "01jbx0000000000000000000n1", "person")); err != nil {
			return err
		}
		if err := RecordUser(ctx, tx, UserDeleted("01jbx0000000000000000000u1")); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	errs := make(chan error, writers)
	for range writers {
		go func() { errs <- write() }()
	}
	for range writers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	store := NewStore(pool)
	tree, _, err := store.ListTree(ctx, 100, 0)
	if err != nil || len(tree) != writers {
		t.Fatalf("tree events: %d %v", len(tree), err)
	}
	users, _, err := store.ListUsers(ctx, 100, 0)
	if err != nil || len(users) != writers {
		t.Fatalf("user events: %d %v", len(users), err)
	}
	// Each writer's pair shares occurred_at, and pairs commit in the same
	// order in both feeds.
	for i := range writers {
		if tree[i].ID != int64(i+1) || users[i].ID != int64(i+1) || !tree[i].OccurredAt.Equal(users[i].OccurredAt) {
			t.Fatalf("event %d: tree %+v, user %+v", i, tree[i], users[i])
		}
	}
}
