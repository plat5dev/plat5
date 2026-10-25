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
		NodeEvent(TypeNodeCreated, "organization", "01jbx0000000000000000000n1", "01jbx0000000000000000000n1"),
		NodeEvent(TypeNodePurged, "project", "01jbx0000000000000000000n2", "01jbx0000000000000000000n1"),
		MemberEvent(TypeMemberRemoved, "01jbx0000000000000000000m1", "01jbx0000000000000000000n1", "person"),
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
	if page[0].Type != TypeNodeCreated || *page[0].Kind != "organization" || page[1].Type != TypeNodePurged || *page[1].Kind != "project" || *page[1].NodeID != "01jbx0000000000000000000n2" {
		t.Fatalf("node events: %+v %+v", page[0], page[1])
	}
	rest, more, err := store.List(ctx, 2, page[1].ID)
	if err != nil || len(rest) != 1 || more {
		t.Fatalf("page 2: %v %v %v", rest, more, err)
	}
	if m := rest[0]; m.Type != TypeMemberRemoved || m.Kind != nil || *m.MemberID != "01jbx0000000000000000000m1" || *m.Principal != "person" || m.NodeID != nil || m.RootID != "01jbx0000000000000000000n1" {
		t.Fatalf("member event: %+v", m)
	}
}
