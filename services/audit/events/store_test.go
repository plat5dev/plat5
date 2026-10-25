package events

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plat5dev/plat5/audit/db"
	"github.com/plat5dev/plat5/audit/internal/id"
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

// testIntent is a service account's event on project 01p of org 01o.
func testIntent(requestID string, at time.Time) *Intent {
	return &Intent{
		RequestID:  requestID,
		OccurredAt: at.UTC().Truncate(time.Millisecond),
		RootID:     strPtr("01o"),
		Nodes:      []Node{{Kind: "organization", ID: "01o"}, {Kind: "project", ID: "01p"}},
		Actor: Actor{MemberID: strPtr("01m"), Principal: strPtr(PrincipalServiceAccount), AuthType: strPtr(AuthAPIKey),
			KeyID: strPtr("01k")},
		Service: "identity",
		Method:  strPtr("PATCH"),
		Route:   strPtr("/orgs/{organization}/projects/{project}/bindings/{binding_id}"),
		Params:  map[string]string{"binding_id": "01b2"},
		IP:      strPtr("203.0.113.7"),
	}
}

// userEvent is a person's event on a user route: no root, no nodes.
func userEvent(requestID string, at time.Time) *Intent {
	return &Intent{
		RequestID:  requestID,
		OccurredAt: at.UTC().Truncate(time.Millisecond),
		Nodes:      []Node{},
		Actor:      Actor{UserID: strPtr("01u"), Principal: strPtr(PrincipalPerson), AuthType: strPtr(AuthJWT)},
		Service:    "identity",
		Method:     strPtr("POST"),
		Route:      strPtr("/orgs"),
		Params:     map[string]string{},
		IP:         strPtr("203.0.113.7"),
	}
}

func intPtr(i int) *int { return &i }

func strPtr(s string) *string { return &s }

func ptrTime(t time.Time) *time.Time { return &t }

func requestIDs(list []*Event) []string {
	var got []string
	for _, e := range list {
		got = append(got, e.RequestID)
	}
	return got
}

func TestIntentIsIdempotentOnRequestID(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	created, err := s.CreateIntent(ctx, testIntent("req-1", now), nil)
	if err != nil || !created {
		t.Fatalf("first intent: %v %v", created, err)
	}
	created, err = s.CreateIntent(ctx, testIntent("req-1", now), nil)
	if err != nil || created {
		t.Fatalf("a retry makes no second event: %v %v", created, err)
	}
	// Even a retry that disagrees on occurred_at is the same request.
	created, err = s.CreateIntent(ctx, testIntent("req-1", now.Add(time.Second)), nil)
	if err != nil || created {
		t.Fatalf("request_id alone is the dedupe: %v %v", created, err)
	}

	list, _, err := s.List(ctx, Filter{Log: "01o", NodeRead: true, Limit: 10})
	if err != nil || len(list) != 1 || list[0].Outcome.Outcome != OutcomePending || list[0].Status != nil {
		t.Fatalf("one pending event: %v %+v", err, list)
	}
	if e := list[0]; e.ID != strings.ToLower(e.ID) || !id.Valid(e.ID) {
		t.Fatalf("the event id is a lowercase ULID: %s", e.ID)
	}
}

func TestOutcomeAppliesOnce(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateIntent(ctx, testIntent("req-1", time.Now()), nil); err != nil {
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

	list, _, err := s.List(ctx, Filter{Log: "01o", NodeRead: true, Limit: 10})
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

// A gateway refusal is one write, final: no outcome changes it.
func TestRejectedIntentIsFinal(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	created, err := s.CreateIntent(ctx, testIntent("req-r", time.Now()), &Outcome{Outcome: OutcomeRejected, Status: intPtr(403)})
	if err != nil || !created {
		t.Fatalf("rejected: %v %v", created, err)
	}
	if r, err := s.ApplyOutcome(ctx, "req-r", &Outcome{Outcome: OutcomeResponded, Status: intPtr(200)}); err != nil || r != metrics.OutcomeFinal {
		t.Fatalf("an outcome on a refusal: %v %v", r, err)
	}
	for _, log := range []string{"01o", "01p"} {
		list, _, err := s.List(ctx, Filter{Log: log, NodeRead: true, Status: intPtr(403), Limit: 10})
		if err != nil || len(list) != 1 || list[0].Outcome.Outcome != OutcomeRejected || *list[0].Status != 403 {
			t.Fatalf("%s's log: %v %+v", log, err, list)
		}
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"pending", "final"} {
		if _, err := s.CreateIntent(ctx, testIntent(id, time.Now()), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ApplyOutcome(ctx, "final", &Outcome{Outcome: OutcomeRejected, Status: intPtr(403)}); err != nil {
		t.Fatal(err)
	}

	for name, sql := range map[string]string{
		"rewrite a final outcome":    `UPDATE audit_events SET status = 200 WHERE request_id = 'final'`,
		"change the actor":           `UPDATE audit_events SET actor_member_id = '01x', outcome = 'rejected', status = 403 WHERE request_id = 'pending'`,
		"change the key":             `UPDATE audit_events SET actor_key_id = '01x', outcome = 'rejected', status = 403 WHERE request_id = 'pending'`,
		"change the chain":           `UPDATE audit_events SET nodes = '[]', outcome = 'rejected', status = 403 WHERE request_id = 'pending'`,
		"set pending without result": `UPDATE audit_events SET status = 200 WHERE request_id = 'pending'`,
		"delete":                     `DELETE FROM audit_events WHERE request_id = 'final'`,
		"move a log key":             `UPDATE audit_log_keys SET log = '01x'`,
		"delete a log key":           `DELETE FROM audit_log_keys`,
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
			in.Actor.MemberID = strPtr("01other")
			in.Params = map[string]string{"binding_id": "01b3"}
		}
		if _, err := s.CreateIntent(ctx, in, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i, status := range []int{200, 403} {
		if _, err := s.ApplyOutcome(ctx, fmt.Sprintf("req-%d", i), &Outcome{Outcome: OutcomeResponded, Status: intPtr(status)}); err != nil {
			t.Fatal(err)
		}
	}
	other := testIntent("req-other-org", base)
	other.RootID = strPtr("01o2")
	other.Nodes = []Node{{Kind: "organization", ID: "01o2"}}
	if _, err := s.CreateIntent(ctx, other, nil); err != nil {
		t.Fatal(err)
	}

	page, hasMore, err := s.List(ctx, Filter{Log: "01o", NodeRead: true, Limit: 2})
	if err != nil || !hasMore || len(page) != 2 || page[0].RequestID != "req-4" || page[1].RequestID != "req-3" {
		t.Fatalf("first page: %v %v %+v", err, hasMore, page)
	}
	page, hasMore, err = s.List(ctx, Filter{Log: "01o", NodeRead: true, Limit: 10, StartingAfter: page[1].ID})
	if err != nil || hasMore || len(page) != 3 || page[0].RequestID != "req-2" || page[2].RequestID != "req-0" {
		t.Fatalf("next page walks to older events: %v %v %+v", err, hasMore, page)
	}

	for name, tc := range map[string]struct {
		f    Filter
		want []string
	}{
		"actor":   {Filter{ActorMemberID: "01other"}, []string{"req-3"}},
		"param":   {Filter{Params: map[string]string{"binding_id": "01b3"}}, []string{"req-3"}},
		"route":   {Filter{Route: "/orgs/{organization}/projects/{project}/bindings/{binding_id}", Method: "PATCH"}, []string{"req-4", "req-3", "req-2", "req-1", "req-0"}},
		"outcome": {Filter{Outcome: OutcomeResponded}, []string{"req-1", "req-0"}},
		"status":  {Filter{Status: intPtr(403)}, []string{"req-1"}},
		"request": {Filter{RequestID: "req-other-org"}, nil},
		"window": {Filter{
			OccurredAfter:  ptrTime(base.Add(time.Minute)),
			OccurredBefore: ptrTime(base.Add(3 * time.Minute)),
		}, []string{"req-2", "req-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			tc.f.Log, tc.f.NodeRead, tc.f.Limit = "01o", true, 10
			list, _, err := s.List(ctx, tc.f)
			if got := requestIDs(list); err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("want %v, got %v (%v)", tc.want, got, err)
			}
		})
	}
}

func TestIntentCreatesMissingPartition(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	// Maintenance covers last month through two ahead. Three ahead is missing.
	far := monthStart(time.Now()).AddDate(0, 3, 1)
	if _, err := s.CreateIntent(ctx, testIntent("req-far", far), nil); err != nil {
		t.Fatalf("intent outside the maintained months: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE request_id = 'req-far'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("stored: %d %v", n, err)
	}
	if list, _, err := s.List(ctx, Filter{Log: "01p", NodeRead: true, Limit: 10}); err != nil || len(list) != 1 {
		t.Fatalf("in its logs: %v %+v", err, list)
	}
}

func TestEnsurePartitionsAcrossMonthEnds(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	// From March 31, last month is February: AddDate on the 31st would skip it.
	if err := s.EnsureAround(ctx, time.Date(2027, 3, 31, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"audit_events_y2027m02", "audit_log_keys_y2027m02"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("%s: %v %v", table, exists, err)
		}
	}
}

// A node's read is its events and everything below it, and the event that
// created it; a person's is their user routes' events. Neither sees another
// tree's.
func TestReadsByNode(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)

	write := func(requestID string, chain ...Node) {
		t.Helper()
		in := testIntent(requestID, base)
		base = base.Add(time.Second)
		in.RootID, in.Nodes = &chain[0].ID, chain
		if _, err := s.CreateIntent(ctx, in, nil); err != nil {
			t.Fatal(err)
		}
	}
	org, web, api := Node{"organization", "01o"}, Node{"project", "01web"}, Node{"project", "01api"}
	env := Node{"environment", "01env"}
	write("on-org", org)
	write("create-web", org)
	if _, err := s.ApplyOutcome(ctx, "create-web", &Outcome{Outcome: OutcomeResponded, Status: intPtr(201), CreatedID: strPtr("01web")}); err != nil {
		t.Fatal(err)
	}
	write("on-web", org, web)
	write("on-api", org, api)
	write("on-env", org, api, env)
	write("other-org", Node{"organization", "01o2"})
	if _, err := s.CreateIntent(ctx, userEvent("user-route", base), nil); err != nil {
		t.Fatal(err)
	}

	for log, want := range map[string][]string{
		"01o":   {"on-env", "on-api", "on-web", "create-web", "on-org"},
		"01web": {"on-web", "create-web"},
		"01api": {"on-env", "on-api"},
		"01env": {"on-env"},
		"01o2":  {"other-org"},
		"01x":   nil,
	} {
		list, _, err := s.List(ctx, Filter{Log: log, NodeRead: true, Limit: 10})
		if got := requestIDs(list); err != nil || !slices.Equal(got, want) {
			t.Fatalf("%s: %v, want %v (%v)", log, got, want, err)
		}
	}
	mine, _, err := s.List(ctx, Filter{Log: UserLog("01u"), Limit: 10})
	if err != nil || len(mine) != 1 || mine[0].RequestID != "user-route" || mine[0].RootID != nil || len(mine[0].Nodes) != 0 ||
		*mine[0].Actor.UserID != "01u" || mine[0].Actor.MemberID != nil {
		t.Fatalf("person's log: %v %+v", err, mine)
	}
	list, _, err := s.List(ctx, Filter{Log: "01env", NodeRead: true, Limit: 10})
	if err != nil || !slices.Equal(list[0].Nodes, []Node{org, api, env}) || *list[0].RootID != "01o" {
		t.Fatalf("chain kept root first: %v %+v", err, list)
	}
}

// A user route that changed a tree is in each named node's log, with the
// named member as its actor there and no user id. The person's log keeps the
// user id.
func TestUserRouteEventIsInTheNamedNodesLogs(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := s.CreateIntent(ctx, userEvent("redeem", now), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateIntent(ctx, testIntent("tree", now), nil); err != nil {
		t.Fatal(err)
	}
	named := &Outcome{Outcome: OutcomeResponded, Status: intPtr(201), NamedNodes: []string{"01n", "01np"}, NamedMemberID: strPtr("01nm")}
	if _, err := s.ApplyOutcome(ctx, "redeem", named); err != nil {
		t.Fatal(err)
	}
	// A tree route's event is in its chain's logs; named nodes are ignored.
	if _, err := s.ApplyOutcome(ctx, "tree", named); err != nil {
		t.Fatal(err)
	}

	mine, _, err := s.List(ctx, Filter{Log: UserLog("01u"), Limit: 10})
	if err != nil || len(mine) != 1 || *mine[0].Actor.UserID != "01u" || mine[0].Actor.MemberID != nil ||
		!slices.Equal(mine[0].NamedNodes, []string{"01n", "01np"}) || *mine[0].NamedMemberID != "01nm" {
		t.Fatalf("person's log: %v %+v", err, mine)
	}
	for _, log := range []string{"01n", "01np"} {
		theirs, _, err := s.List(ctx, Filter{Log: log, NodeRead: true, Limit: 10})
		if err != nil || len(theirs) != 1 || theirs[0].RequestID != "redeem" || *theirs[0].Actor.MemberID != "01nm" || theirs[0].Actor.UserID != nil {
			t.Fatalf("%s's log: %v %+v", log, err, theirs)
		}
		for actor, want := range map[string]int{"01nm": 1, "01u": 0} {
			list, _, err := s.List(ctx, Filter{Log: log, NodeRead: true, ActorMemberID: actor, Limit: 10})
			if err != nil || len(list) != want {
				t.Fatalf("actor %s in %s's log: %v %+v", actor, log, err, list)
			}
		}
	}
	tree, _, err := s.List(ctx, Filter{Log: "01o", NodeRead: true, Limit: 10})
	if err != nil || len(tree) != 1 || len(tree[0].NamedNodes) != 0 || tree[0].NamedMemberID != nil || *tree[0].Actor.MemberID != "01m" {
		t.Fatalf("tree event unchanged: %v %+v", err, tree)
	}

	if _, err := pool.Exec(ctx, `UPDATE audit_events SET named_nodes = '{01x}' WHERE request_id = 'redeem'`); err == nil {
		t.Fatal("a final event's named nodes changed")
	}
}

// The table has no checks between fields: what the gateway sends is stored
// and read back as sent.
func TestStoresWhatItsSent(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	in := &Intent{
		RequestID:  "odd",
		OccurredAt: time.Now().UTC().Truncate(time.Millisecond),
		Nodes:      []Node{{Kind: "organization", ID: "01o"}},
		Actor:      Actor{MemberID: strPtr("01m"), UserID: strPtr("01u"), Principal: strPtr("robot"), AuthType: strPtr(AuthJWT), KeyID: strPtr("01k")},
		Service:    "e10s",
		Params:     map[string]string{},
	}
	in.RootID = &in.Nodes[0].ID
	if _, err := s.CreateIntent(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyOutcome(ctx, "odd", &Outcome{Outcome: "timed_out", Details: json.RawMessage(`{"a":1}`), CreatedID: strPtr("01c")}); err != nil {
		t.Fatal(err)
	}
	list, _, err := s.List(ctx, Filter{Log: "01c", NodeRead: true, Limit: 10})
	if err != nil || len(list) != 1 {
		t.Fatalf("created_id's log: %v %+v", err, list)
	}
	e := list[0]
	if *e.Actor.Principal != "robot" || *e.Actor.KeyID != "01k" || e.Method != nil || e.Route != nil || e.IP != nil ||
		e.Outcome.Outcome != "timed_out" || e.Status != nil {
		t.Fatalf("stored as sent: %+v", e)
	}
}
