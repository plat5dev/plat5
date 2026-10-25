package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/audit/errors"
)

// Audit's expanded routes, as the control plane feeds them to the gateway,
// against audit: every upstream is served with the ids the gateway fills,
// and each read returns its log. The fixture's tree is organization, project,
// environment, one parent each (control-plane/fixtures/catalog). Permissions
// are decided by admit and the gateway, and tested there.

const gatewayFeed = "../../control-plane/fixtures/catalog/gateway-feed.json"

type feedRoute struct {
	Path     string `json:"path"`
	Method   string `json:"method"`
	Scope    string `json:"scope"`
	Upstream string `json:"upstream"`
}

var placeholderRe = regexp.MustCompile(`\{([a-z_]+)\.id\}`)

func auditRoutes(t *testing.T) []feedRoute {
	t.Helper()
	raw, err := os.ReadFile(gatewayFeed)
	if err != nil {
		t.Fatal(err)
	}
	var feed struct {
		Services map[string]struct {
			Routes []feedRoute `json:"routes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(raw, &feed); err != nil {
		t.Fatal(err)
	}
	routes := feed.Services["audit"].Routes
	if len(routes) == 0 {
		t.Fatal("no audit routes in the gateway feed")
	}
	return routes
}

func TestFeedEveryAuditUpstreamIsServed(t *testing.T) {
	s, _ := newTestStore(t)
	h := &Handler{store: s}
	app := fiber.New(fiber.Config{ErrorHandler: errors.FiberErrorHandler})
	h.MountPublic(app)
	h.MountInternal(app.Group("/internal"))

	// The ids the gateway fills: each chain node's, and the caller's user id.
	ids := map[string]string{
		"organization": "01jbw0rg000000000000000000",
		"project":      "01jbx0pr000000000000000000",
		"environment":  "01jbx1en000000000000000000",
		"user":         "01jbu0se000000000000000000",
	}
	const member = "01jby0me000000000000000000"
	org := Node{"organization", ids["organization"]}
	project := Node{"project", ids["project"]}
	env := Node{"environment", ids["environment"]}
	at := time.Now().Format(time.RFC3339Nano)

	put := func(requestID, body string) {
		t.Helper()
		if code, resp := do(t, app, http.MethodPut, "/internal/events/"+requestID, body); code != http.StatusCreated {
			t.Fatalf("intent %s: %d %s", requestID, code, resp)
		}
	}
	patch := func(requestID, body string) {
		t.Helper()
		if code, resp := do(t, app, http.MethodPatch, "/internal/events/"+requestID, body); code != http.StatusNoContent {
			t.Fatalf("outcome %s: %d %s", requestID, code, resp)
		}
	}
	treeIntent := func(chain []Node, extra string) string {
		nodes, _ := json.Marshal(chain)
		return fmt.Sprintf(`{"occurred_at": %q, "nodes": %s,
			"actor": {"member_id": %q, "user_id": null, "principal": "person", "auth_type": "jwt", "key_id": null},
			"service": "e10s", "method": "POST", "route": "/x", "params": {}, "ip": "203.0.113.7", "user_agent": null%s}`,
			at, nodes, member, extra)
	}

	// The person creates the org on a user route; the service names it.
	put("create-org", fmt.Sprintf(`{"occurred_at": %q, "nodes": [],
		"actor": {"member_id": null, "user_id": %q, "principal": "person", "auth_type": "jwt", "key_id": null},
		"service": "identity", "method": "POST", "route": "/orgs", "params": {}, "ip": "203.0.113.7", "user_agent": null}`,
		at, ids["user"]))
	patch("create-org", fmt.Sprintf(`{"outcome": "responded", "status": 201, "details": null, "created_id": %q,
		"named_nodes": [%q], "named_member_id": %q}`, org.ID, org.ID, member))
	// A project created on the org, then events on each node, and a refusal.
	put("create-project", treeIntent([]Node{org}, ""))
	patch("create-project", fmt.Sprintf(`{"outcome": "responded", "status": 201, "details": null, "created_id": %q}`, project.ID))
	put("on-org", treeIntent([]Node{org}, ""))
	put("on-project", treeIntent([]Node{org, project}, ""))
	put("on-env", treeIntent([]Node{org, project, env}, ""))
	put("refused-env", treeIntent([]Node{org, project, env}, `, "outcome": "rejected", "status": 403`))

	want := map[string][]string{
		"organization": {"create-org", "create-project", "on-env", "on-org", "on-project", "refused-env"},
		"project":      {"create-project", "on-env", "on-project", "refused-env"},
		"environment":  {"on-env", "refused-env"},
		"user":         {"create-org"},
	}
	for _, r := range auditRoutes(t) {
		upstream := placeholderRe.ReplaceAllStringFunc(r.Upstream, func(p string) string {
			v, ok := ids[placeholderRe.FindStringSubmatch(p)[1]]
			if !ok {
				t.Fatalf("%s: nothing fills %s in %s", r.Path, p, r.Upstream)
			}
			return v
		})
		code, body := do(t, app, r.Method, upstream, "")
		if code != http.StatusOK {
			t.Fatalf("%s %s (upstream %s): %d %s", r.Method, r.Path, upstream, code, body)
		}
		var resp struct {
			AuditEvents []struct {
				RequestID string `json:"request_id"`
				Actor     Actor  `json:"actor"`
			} `json:"audit_events"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range resp.AuditEvents {
			got = append(got, e.RequestID)
			// In a node's read the actor is the member; a user id stays in
			// the person's own log.
			mine := r.Scope == "user"
			if (e.Actor.UserID != nil) != mine || (e.Actor.MemberID != nil) == mine {
				t.Fatalf("%s: %s's actor is %+v", r.Path, e.RequestID, e.Actor)
			}
			if !mine && *e.Actor.MemberID != member || mine && *e.Actor.UserID != ids["user"] {
				t.Fatalf("%s: %s's actor is %+v", r.Path, e.RequestID, e.Actor)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, want[r.Scope]) {
			t.Fatalf("%s %s: %v, want %v", r.Method, r.Path, got, want[r.Scope])
		}
		delete(want, r.Scope)
	}
	if len(want) != 0 {
		t.Fatalf("no audit route reads %v", want)
	}
}
