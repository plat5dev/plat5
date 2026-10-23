package events

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"os"
	"slices"
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

// testIntent is a service account's event on a project of org n_org.
func testIntent(requestID string, at time.Time) *Intent {
	return &Intent{
		RequestID:  requestID,
		OccurredAt: at.UTC().Truncate(time.Millisecond),
		RootID:     strPtr("n_org"),
		Nodes:      []Node{{Kind: "organization", ID: "n_org"}, {Kind: "project", ID: "n_web"}},
		Actor:      Actor{MemberID: "m_1", Principal: PrincipalServiceAccount, AuthType: AuthAPIKey, KeyPrefix: strPtr("plat5-k-2-AbC9")},
		Service:    "identity",
		Method:     "PATCH",
		Route:      "/orgs/{organization}/projects/{project}/bindings/{binding_id}",
		Params:     map[string]string{"binding_id": "b_2"},
		IP:         "203.0.113.7",
	}
}

// signInEvent is a person's event on a sign-in route: no root, no nodes.
func signInEvent(requestID string, at time.Time) *Intent {
	return &Intent{
		RequestID:  requestID,
		OccurredAt: at.UTC().Truncate(time.Millisecond),
		Nodes:      []Node{},
		Actor:      Actor{MemberID: "m_dir", Principal: PrincipalPerson, AuthType: AuthJWT},
		Service:    "identity",
		Method:     "POST",
		Route:      "/orgs",
		Params:     map[string]string{},
		IP:         "203.0.113.7",
	}
}

func intPtr(i int) *int { return &i }

func strPtr(s string) *string { return &s }

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

	list, _, err := s.List(ctx, Filter{NodeID: "n_org", Limit: 10})
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

	list, _, err := s.List(ctx, Filter{NodeID: "n_org", Limit: 10})
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
		"rewrite a final outcome":     `UPDATE audit_events SET status = 200 WHERE request_id = 'final'`,
		"change the actor":            `UPDATE audit_events SET actor_member_id = 'm_9', outcome = 'rejected', status = 403 WHERE request_id = 'pending'`,
		"change the chain":            `UPDATE audit_events SET node_ids = ARRAY['n_org'], outcome = 'rejected', status = 403 WHERE request_id = 'pending'`,
		"name a tree on a kind event": `UPDATE audit_events SET named_root_id = 'n_x', named_member_id = 'm_x', outcome = 'rejected', status = 403 WHERE request_id = 'pending'`,
		"set pending without result":  `UPDATE audit_events SET status = 200 WHERE request_id = 'pending'`,
		"delete":                      `DELETE FROM audit_events WHERE request_id = 'final'`,
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
			in.Actor.MemberID = "m_other"
			in.Params = map[string]string{"binding_id": "b_3"}
		}
		if _, err := s.CreateIntent(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	other := testIntent("req-other-org", base)
	other.RootID = strPtr("n_org2")
	other.Nodes = []Node{{Kind: "organization", ID: "n_org2"}}
	if _, err := s.CreateIntent(ctx, other); err != nil {
		t.Fatal(err)
	}

	page, hasMore, err := s.List(ctx, Filter{NodeID: "n_org", Limit: 2})
	if err != nil || !hasMore || len(page) != 2 || page[0].RequestID != "req-4" || page[1].RequestID != "req-3" {
		t.Fatalf("first page: %v %v %+v", err, hasMore, page)
	}
	page, hasMore, err = s.List(ctx, Filter{NodeID: "n_org", Limit: 10, StartingAfter: page[1].ID})
	if err != nil || hasMore || len(page) != 3 || page[0].RequestID != "req-2" || page[2].RequestID != "req-0" {
		t.Fatalf("next page walks to older events: %v %v %+v", err, hasMore, page)
	}

	for name, tc := range map[string]struct {
		f    Filter
		want int
	}{
		"actor":   {Filter{ActorMemberID: "m_other"}, 1},
		"param":   {Filter{Params: map[string]string{"binding_id": "b_3"}}, 1},
		"route":   {Filter{Route: "/orgs/{organization}/projects/{project}/bindings/{binding_id}", Method: "PATCH"}, 5},
		"outcome": {Filter{Outcome: OutcomeResponded}, 0},
		"request": {Filter{RequestID: "req-other-org"}, 0},
		"window": {Filter{
			OccurredAfter:  ptrTime(base.Add(time.Minute)),
			OccurredBefore: ptrTime(base.Add(3 * time.Minute)),
		}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			tc.f.NodeID, tc.f.Limit = "n_org", 10
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

// A node's read is its events and everything below it; a person's is their
// sign-in route events. Neither sees another tree's.
func TestReadsByNode(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)

	write := func(requestID string, root string, chain ...Node) {
		t.Helper()
		in := testIntent(requestID, base)
		base = base.Add(time.Second)
		in.RootID, in.Nodes = &root, chain
		if _, err := s.CreateIntent(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	org, web, api := Node{"organization", "n_org"}, Node{"project", "n_web"}, Node{"project", "n_api"}
	eng := Node{"folder", "n_eng"}
	write("on-org", "n_org", org)
	write("on-web", "n_org", org, web)
	write("on-eng", "n_org", org, eng)
	write("on-api", "n_org", org, eng, api)
	write("other-org", "n_org2", Node{"organization", "n_org2"})
	if _, err := s.CreateIntent(ctx, signInEvent("sign-in", base)); err != nil {
		t.Fatal(err)
	}

	for node, want := range map[string][]string{
		"n_org":  {"on-api", "on-eng", "on-web", "on-org"},
		"n_eng":  {"on-api", "on-eng"},
		"n_api":  {"on-api"},
		"n_web":  {"on-web"},
		"n_org2": {"other-org"},
		"n_none": nil,
	} {
		list, _, err := s.List(ctx, Filter{NodeID: node, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range list {
			got = append(got, e.RequestID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s: %v, want %v", node, got, want)
		}
	}
	mine, _, err := s.List(ctx, Filter{MemberID: "m_dir", Limit: 10})
	if err != nil || len(mine) != 1 || mine[0].RequestID != "sign-in" || mine[0].RootID != nil || len(mine[0].Nodes) != 0 {
		t.Fatalf("person's log: %v %+v", err, mine)
	}
	// A kind route's event is never in a person's log, even its actor's.
	if list, _, err := s.List(ctx, Filter{MemberID: "m_1", Limit: 10}); err != nil || len(list) != 0 {
		t.Fatalf("m_1's own log: %v %+v", err, list)
	}
	list, _, err := s.List(ctx, Filter{NodeID: "n_api", Limit: 10})
	if err != nil || !slices.Equal(list[0].Nodes, []Node{org, eng, api}) || *list[0].RootID != "n_org" {
		t.Fatalf("chain kept root first: %v %+v", err, list)
	}
}

// A sign-in route that changed a tree is in that root's log too, with the
// named member as its actor there. The person's log keeps their directory
// member.
func TestSignInEventIsInTheNamedRootsLog(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := s.CreateIntent(ctx, signInEvent("req-create", now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateIntent(ctx, testIntent("req-kind", now)); err != nil {
		t.Fatal(err)
	}
	named := &Outcome{Outcome: OutcomeResponded, Status: intPtr(201), CreatedID: strPtr("n_new"),
		Named: &Named{RootID: "n_new", MemberID: "m_new"}}
	if _, err := s.ApplyOutcome(ctx, "req-create", named); err != nil {
		t.Fatal(err)
	}
	// A kind route's event is in its chain's root's log; a named pair is ignored.
	if _, err := s.ApplyOutcome(ctx, "req-kind", named); err != nil {
		t.Fatal(err)
	}

	mine, _, err := s.List(ctx, Filter{MemberID: "m_dir", Limit: 10})
	if err != nil || len(mine) != 1 || mine[0].Actor.MemberID != "m_dir" || mine[0].Named == nil ||
		mine[0].Named.RootID != "n_new" || mine[0].Named.MemberID != "m_new" || *mine[0].CreatedID != "n_new" {
		t.Fatalf("person's log: %v %+v", err, mine)
	}
	theirs, _, err := s.List(ctx, Filter{NodeID: "n_new", Limit: 10})
	if err != nil || len(theirs) != 1 || theirs[0].RequestID != "req-create" || theirs[0].Actor.MemberID != "m_new" {
		t.Fatalf("the named root's log: %v %+v", err, theirs)
	}
	for actor, want := range map[string]int{"m_new": 1, "m_dir": 0} {
		list, _, err := s.List(ctx, Filter{NodeID: "n_new", ActorMemberID: actor, Limit: 10})
		if err != nil || len(list) != want {
			t.Fatalf("actor %s in the root's log: %v %+v", actor, err, list)
		}
	}
	byCreated, _, err := s.List(ctx, Filter{NodeID: "n_new", CreatedID: "n_new", Limit: 10})
	if err != nil || len(byCreated) != 1 {
		t.Fatalf("created_id filter: %v %+v", err, byCreated)
	}
	kind, _, err := s.List(ctx, Filter{NodeID: "n_org", Limit: 10})
	if err != nil || len(kind) != 1 || kind[0].Named != nil || kind[0].Actor.MemberID != "m_1" {
		t.Fatalf("kind event unchanged: %v %+v", err, kind)
	}
	if list, _, err := s.List(ctx, Filter{NodeID: "n_new", Limit: 10}); err != nil || len(list) != 1 {
		t.Fatalf("the kind event isn't in the named root's log: %v %+v", err, list)
	}

	if _, err := pool.Exec(ctx, `UPDATE audit_events SET named_root_id = 'n_x' WHERE request_id = 'req-create'`); err == nil {
		t.Fatal("a final event's named root changed")
	}
}

// The table refuses rows that can't describe one request, whatever writes them.
func TestEventShapeConstraints(t *testing.T) {
	_, pool := newTestStore(t)
	ctx := context.Background()
	insert := `INSERT INTO audit_events (id, occurred_at, request_id, root_id, nodes, node_ids, actor_member_id,
		actor_principal, actor_auth_type, actor_key_prefix, service, method, route, ip)
		VALUES ('01JA2Z6Q3Y8D5V2K9N4R7T1W0X', now(), $1, $2, '[]', $3, 'm_1', 'person', $4, $5, 's', 'POST', '/r', '127.0.0.1')`
	for name, args := range map[string][]any{
		"root without nodes":  {"a", "n_org", []string{}, "jwt", nil},
		"nodes without root":  {"b", nil, []string{"n_org"}, "jwt", nil},
		"root isn't the head": {"c", "n_web", []string{"n_org", "n_web"}, "jwt", nil},
		"key without prefix":  {"d", nil, []string{}, "apikey", nil},
		"prefix on a jwt":     {"e", nil, []string{}, "jwt", "p"},
	} {
		if _, err := pool.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s: inserted", name)
		}
	}
	if _, err := pool.Exec(ctx, insert, "ok", "n_org", []string{"n_org", "n_web"}, "apikey", "p"); err != nil {
		t.Fatalf("a well-formed row: %v", err)
	}
}
