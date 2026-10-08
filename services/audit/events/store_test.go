package events

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plat5dev/plat5/audit/db"
	"github.com/plat5dev/plat5/audit/metrics"
)

// newTestStore migrates a fresh schema on AUDIT_TEST_DATABASE_URL, or skips.
// The append-only rules live in SQL, so these run against Postgres.
func newTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("AUDIT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AUDIT_TEST_DATABASE_URL is unset")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("audit_test_%d", time.Now().UnixNano())
	pool, err := db.ConnectSchema(ctx, url, schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		pool.Close()
	})
	if err := db.MigrateSchema(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	s := NewStore(pool)
	if err := s.EnsureAround(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	return s, pool
}

func testIntent(requestID string, at time.Time) *Intent {
	return &Intent{
		RequestID:      requestID,
		OccurredAt:     at.UTC().Truncate(time.Millisecond),
		OrganizationID: "org-1",
		Actor:          Actor{MemberID: "m-1", AuthType: AuthMemberAPIKey, KeyPrefix: "plat5-mk-1-AbC9"},
		Service:        "identity",
		Method:         "PATCH",
		Route:          "/org/members/{member_id}",
		Params:         map[string]string{"member_id": "m-2"},
		IP:             "203.0.113.7",
	}
}

func intPtr(i int) *int { return &i }

func TestIntentIsIdempotentOnRequestID(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	created, err := s.CreateIntent(ctx, testIntent("req-1", now))
	if err != nil || !created {
		t.Fatalf("first intent: %v %v", created, err)
	}
	created, err = s.CreateIntent(ctx, testIntent("req-1", now))
	if err != nil || created {
		t.Fatalf("a retry makes no second event: %v %v", created, err)
	}
	// Even a retry that disagrees on occurred_at is the same request.
	created, err = s.CreateIntent(ctx, testIntent("req-1", now.Add(time.Second)))
	if err != nil || created {
		t.Fatalf("request_id alone is the dedupe: %v %v", created, err)
	}

	list, _, err := s.List(ctx, Filter{OrganizationID: "org-1", Limit: 10})
	if err != nil || len(list) != 1 || list[0].Outcome.Outcome != OutcomePending || list[0].Status != nil {
		t.Fatalf("one pending event: %v %+v", err, list)
	}
}

func TestOutcomeAppliesOnce(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateIntent(ctx, testIntent("req-1", time.Now())); err != nil {
		t.Fatal(err)
	}

	first := &Outcome{Outcome: OutcomeResponded, Status: intPtr(200), Details: json.RawMessage(`{"role":{"from":"developer","to":"admin"}}`)}
	if r, err := s.ApplyOutcome(ctx, "req-1", first); err != nil || r != metrics.OutcomeApplied {
		t.Fatalf("apply: %v %v", r, err)
	}
	if r, err := s.ApplyOutcome(ctx, "req-1", &Outcome{Outcome: OutcomeNoResponse, Status: intPtr(502)}); err != nil || r != metrics.OutcomeFinal {
		t.Fatalf("a second outcome changes nothing: %v %v", r, err)
	}
	if _, err := s.ApplyOutcome(ctx, "nope", first); !stderrors.Is(err, ErrNotFound) {
		t.Fatalf("no intent: %v", err)
	}

	list, _, err := s.List(ctx, Filter{OrganizationID: "org-1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	e := list[0]
	var details map[string]map[string]string
	if err := json.Unmarshal(e.Details, &details); err != nil {
		t.Fatal(err)
	}
	if e.Outcome.Outcome != OutcomeResponded || *e.Status != 200 || details["role"]["to"] != "admin" {
		t.Fatalf("the first outcome stands: %+v %s", e.Outcome, e.Details)
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"pending", "final"} {
		if _, err := s.CreateIntent(ctx, testIntent(id, time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ApplyOutcome(ctx, "final", &Outcome{Outcome: OutcomeRejected, Status: intPtr(403)}); err != nil {
		t.Fatal(err)
	}

	for name, sql := range map[string]string{
		"rewrite a final outcome":    `UPDATE audit_events SET status = 200 WHERE request_id = 'final'`,
		"change the actor":           `UPDATE audit_events SET actor_member_id = 'm-9', outcome = 'rejected', status = 403 WHERE request_id = 'pending'`,
		"set pending without result": `UPDATE audit_events SET status = 200 WHERE request_id = 'pending'`,
		"delete":                     `DELETE FROM audit_events WHERE request_id = 'final'`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, sql); err == nil {
				t.Fatal("the guard trigger allowed it")
			}
		})
	}
}

func TestListNewestFirstWithFilters(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)

	for i := 0; i < 5; i++ {
		in := testIntent(fmt.Sprintf("req-%d", i), base.Add(time.Duration(i)*time.Minute))
		if i == 3 {
			in.Actor.MemberID = "m-other"
			in.Params = map[string]string{"member_id": "m-3"}
		}
		if _, err := s.CreateIntent(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	other := testIntent("req-other-org", base)
	other.OrganizationID = "org-2"
	if _, err := s.CreateIntent(ctx, other); err != nil {
		t.Fatal(err)
	}

	page, hasMore, err := s.List(ctx, Filter{OrganizationID: "org-1", Limit: 2})
	if err != nil || !hasMore || len(page) != 2 || page[0].RequestID != "req-4" || page[1].RequestID != "req-3" {
		t.Fatalf("first page: %v %v %+v", err, hasMore, page)
	}
	page, hasMore, err = s.List(ctx, Filter{OrganizationID: "org-1", Limit: 10, StartingAfter: page[1].ID})
	if err != nil || hasMore || len(page) != 3 || page[0].RequestID != "req-2" || page[2].RequestID != "req-0" {
		t.Fatalf("next page walks to older events: %v %v %+v", err, hasMore, page)
	}

	for name, tc := range map[string]struct {
		f    Filter
		want int
	}{
		"actor":   {Filter{ActorMemberID: "m-other"}, 1},
		"param":   {Filter{Params: map[string]string{"member_id": "m-3"}}, 1},
		"route":   {Filter{Route: "/org/members/{member_id}", Method: "PATCH"}, 5},
		"outcome": {Filter{Outcome: OutcomeResponded}, 0},
		"request": {Filter{RequestID: "req-other-org"}, 0},
		"window": {Filter{
			OccurredAfter:  ptrTime(base.Add(time.Minute)),
			OccurredBefore: ptrTime(base.Add(3 * time.Minute)),
		}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			tc.f.OrganizationID, tc.f.Limit = "org-1", 10
			list, _, err := s.List(ctx, tc.f)
			if err != nil || len(list) != tc.want {
				t.Fatalf("want %d, got %d (%v)", tc.want, len(list), err)
			}
		})
	}
}

func TestIntentCreatesMissingPartition(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	// Maintenance covers last month through two ahead. Three ahead is missing.
	far := monthStart(time.Now()).AddDate(0, 3, 1)
	if _, err := s.CreateIntent(ctx, testIntent("req-far", far)); err != nil {
		t.Fatalf("intent outside the maintained months: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE request_id = 'req-far'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("stored: %d %v", n, err)
	}
}

func TestEnsurePartitionsAcrossMonthEnds(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	// From March 31, last month is February: AddDate on the 31st would skip it.
	if err := s.EnsureAround(ctx, time.Date(2027, 3, 31, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('audit_events_y2027m02') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("February partition: %v %v", exists, err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
