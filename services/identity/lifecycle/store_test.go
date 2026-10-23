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
	for _, e := range []Event{
		NodeEvent(TypeNodeCreated, "organization", "n_1", "n_1"),
		NodeEvent(TypeNodePurged, "project", "n_2", "n_1"),
		MemberEvent(TypeMemberRemoved, "m_1", "n_1", "person"),
	} {
		if err := Record(ctx, tx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	store := NewStore(pool)
	page, more, err := store.List(ctx, 2, 0)
	if err != nil || len(page) != 2 || !more {
		t.Fatalf("page 1: %v %v %v", page, more, err)
	}
	if page[0].WireType() != "organization.created" || page[1].WireType() != "project.purged" || *page[1].NodeID != "n_2" {
		t.Fatalf("node events: %s %s", page[0].WireType(), page[1].WireType())
	}
	rest, more, err := store.List(ctx, 2, page[1].ID)
	if err != nil || len(rest) != 1 || more {
		t.Fatalf("page 2: %v %v %v", rest, more, err)
	}
	if m := rest[0]; m.WireType() != "member.removed" || *m.MemberID != "m_1" || *m.Principal != "person" || m.NodeID != nil || m.RootID != "n_1" {
		t.Fatalf("member event: %+v", m)
	}
}
