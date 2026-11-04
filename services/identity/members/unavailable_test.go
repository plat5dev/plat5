package members

import (
	"context"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"

	apierrors "github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/nodes"
)

// Postgres unavailable is 503 SERVICE_UNAVAILABLE on every port: the service
// API, admit, plan counts and access evaluation
// (docs/reference/errors.md#which-status). It needs no Postgres.
func TestPostgresDownIs503(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://plat5:plat5@127.0.0.1:1/plat5?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	snap, err := feed.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	src := feed.Static(snap)
	nodeStore := nodes.NewStore(pool)
	store := NewStore(pool, nodeStore, "plat5")
	e := &env{t: t, ctx: context.Background(), pool: pool, nodes: nodeStore, store: store, src: src,
		clock: &clock{t: time.Now()}}
	e.app = fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	nodes.NewHandler(nodeStore, src).Mount(e.app)
	NewHandler(store, src).Mount(e.app)
	e.internal = fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	NewHandler(store, src).MountInternal(e.internal.Group("/internal"))
	nodes.NewHandler(nodeStore, src).MountInternal(e.internal.Group("/internal"))
	NewHandler(store, src).MountAccess(e.internal.Group("/access/v1"))

	node, member, user := id.New(), id.New(), id.New()
	for _, c := range []struct {
		name string
		r    func() response
	}{
		{"get a node", func() response { return e.call("GET", "/nodes/"+node, "", "") }},
		{"list children", func() response { return e.call("GET", "/nodes/"+node+"/children/folder", "", "") }},
		{"create a root", func() response { return e.call("POST", "/roots/organization", "", `{"display_name": "Gruow"}`) }},
		{"list members", func() response { return e.call("GET", "/nodes/"+node+"/members", "", "") }},
		{"set a name", func() response { return e.call("PATCH", "/users/"+user, "", `{"name": "Dana"}`) }},
		{"redeem", func() response {
			return e.call("POST", "/users/"+user+"/invites/redeem", "", `{"token": "plat5-i-1-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`)
		}},
		{"admit", func() response {
			return e.callInternal("POST", "/internal/admit", `{"idp": {"issuer": "https://idp.test", "subject": "dana"}, "chain": []}`)
		}},
		{"plan counts", func() response { return e.callInternal("POST", "/internal/plan", `{"kinds": ["folder"]}`) }},
		{"evaluation", func() response {
			return e.callInternal("POST", "/access/v1/evaluation", `{"subject": {"type": "member", "id": "`+member+`"},
				"resource": {"type": "project", "id": "`+node+`"}, "action": {"name": "project:read"}}`)
		}},
	} {
		r := c.r()
		if r.status != 503 || r.code() != "SERVICE_UNAVAILABLE" || r.message() != "Service temporarily unavailable." {
			t.Fatalf("%s: %d %v", c.name, r.status, r.body)
		}
	}
}
