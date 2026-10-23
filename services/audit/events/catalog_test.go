package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/audit/errors"
)

// The generated catalog's audit routes (docs/reference/api/edge-catalog.md)
// against audit: every upstream is served, with the ids the gateway fills, and
// each read returns its node's events and those below it. The routes are the
// control plane's own output for a kind with two parents
// (control-plane/fixtures/catalog). Identity's catalog test checks that their
// labels reach them.

const catalogFeed = "../../control-plane/fixtures/catalog/identity-feed.json"

type catalogRoute struct {
	Path     string   `json:"path"`
	Scope    string   `json:"scope"`
	Upstream string   `json:"upstream"`
	Methods  []string `json:"methods"`
}

var paramRe = regexp.MustCompile(`\{([a-z_.]+)\}`)

func auditCatalog(t *testing.T) []catalogRoute {
	t.Helper()
	raw, err := os.ReadFile(catalogFeed)
	if err != nil {
		t.Fatal(err)
	}
	var feed struct {
		Services map[string]struct {
			Routes []catalogRoute `json:"routes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(raw, &feed); err != nil {
		t.Fatal(err)
	}
	routes := feed.Services["audit"].Routes
	if len(routes) == 0 {
		t.Fatal("no audit routes in the catalog")
	}
	return routes
}

func TestCatalogEveryAuditUpstreamIsServed(t *testing.T) {
	s, _ := newTestStore(t)
	h := &Handler{store: s, now: time.Now}
	app := fiber.New(fiber.Config{ErrorHandler: errors.FiberErrorHandler})
	h.MountPublic(app)
	h.MountInternal(app.Group("/internal"))

	// The fixture's tree: org gruow, folder eng, project web under the org and
	// project api under the folder. Each node has one event, written as the
	// gateway writes it; dana's sign-in event created the org.
	ids := map[string]string{"organization": "n_gruow", "folder": "n_eng", "web": "n_web", "api": "n_api"}
	chains := map[string][]Node{
		"on-org":    {{"organization", "n_gruow"}},
		"on-folder": {{"organization", "n_gruow"}, {"folder", "n_eng"}},
		"on-web":    {{"organization", "n_gruow"}, {"project", "n_web"}},
		"on-api":    {{"organization", "n_gruow"}, {"folder", "n_eng"}, {"project", "n_api"}},
	}
	at := time.Now()
	for req, chain := range chains {
		nodes, _ := json.Marshal(chain)
		intent := fmt.Sprintf(`{"occurred_at": %q, "root_id": "n_gruow", "nodes": %s,
			"actor": {"member_id": "m_dana_org", "principal": "person", "auth_type": "jwt"},
			"service": "e10s", "method": "POST", "route": "/x", "ip": "203.0.113.7"}`, at.Format(time.RFC3339Nano), nodes)
		if code, body := do(t, app, http.MethodPut, "/internal/events/"+req, intent); code != http.StatusCreated {
			t.Fatalf("intent %s: %d %s", req, code, body)
		}
	}
	signIn := fmt.Sprintf(`{"occurred_at": %q, "root_id": null, "nodes": [],
		"actor": {"member_id": "m_dana", "principal": "person", "auth_type": "jwt"},
		"service": "identity", "method": "POST", "route": "/orgs", "ip": "203.0.113.7"}`, at.Format(time.RFC3339Nano))
	if code, body := do(t, app, http.MethodPut, "/internal/events/sign-in", signIn); code != http.StatusCreated {
		t.Fatalf("sign-in intent: %d %s", code, body)
	}
	named := `{"outcome": "responded", "status": 201, "created_id": "n_gruow", "root_id": "n_gruow", "member_id": "m_dana_org"}`
	if code, body := do(t, app, http.MethodPatch, "/internal/events/sign-in", named); code != http.StatusNoContent {
		t.Fatalf("sign-in outcome: %d %s", code, body)
	}

	// What each scope's read returns: its node's events and those below.
	want := map[string][]string{
		"organization": {"on-api", "on-folder", "on-org", "on-web", "sign-in"},
		"folder":       {"on-api", "on-folder"},
		"web":          {"on-web"},
		"api":          {"on-api"},
		"directory":    {"sign-in"},
	}
	served := map[string]bool{}
	for _, r := range auditCatalog(t) {
		// The subject the gateway fills: each chain node's id, and on the
		// sign-in kind the caller's directory member.
		subject := map[string]string{"subject.member_id": "m_dana"}
		for _, m := range paramRe.FindAllStringSubmatch(r.Path, -1) {
			subject["subject."+m[1]+"_id"] = ids[m[1]]
		}
		scope := r.Scope
		if scope == "project" {
			scope = "web"
			if strings.Contains(r.Path, "{folder}") {
				scope = "api"
			}
			subject["subject.project_id"] = ids[scope]
		}
		upstream := paramRe.ReplaceAllStringFunc(r.Upstream, func(p string) string {
			v, ok := subject[p[1:len(p)-1]]
			if !ok {
				t.Fatalf("%s: nothing fills %s in %s", r.Path, p, r.Upstream)
			}
			return v
		})
		for _, method := range r.Methods {
			code, body := do(t, app, method, upstream, "")
			if code != http.StatusOK {
				t.Fatalf("%s %s (upstream %s): %d %s", method, r.Path, upstream, code, body)
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
				// The directory member stays in the person's own log.
				if e.RequestID == "sign-in" && (scope == "directory") != (e.Actor.MemberID == "m_dana") {
					t.Fatalf("%s: sign-in event's actor is %s", r.Path, e.Actor.MemberID)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, want[scope]) {
				t.Fatalf("%s %s: %v, want %v", method, r.Path, got, want[scope])
			}
			served[method+" "+r.Path] = true
		}
	}
	for _, r := range auditCatalog(t) {
		for _, m := range r.Methods {
			if !served[m+" "+r.Path] {
				t.Errorf("%s %s (upstream %s) was never served", m, r.Path, r.Upstream)
			}
		}
	}
}
