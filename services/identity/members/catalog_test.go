package members

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/plat5dev/plat5/identity/nodes"
)

// Identity's resources, expanded by the control plane into the routes the
// gateway serves, against identity: every upstream is served with its
// documented status, and every permission-gated route admits a member whose
// only grant is that permission. The routes are the control plane's own
// output for organization > project > environment
// (control-plane/fixtures/catalog): gateway-feed.json's identity routes, and
// identity-feed.json for the config identity reads. Each request goes the way
// the gateway sends it (docs/concepts/access.md#how-a-request-is-decided):
// match, admit with the path's chain, permissions, fill the upstream, forward
// with the caller's header. Audit's routes belong to the audit service.

const (
	catalogIdentityFeed = "../../control-plane/fixtures/catalog/identity-feed.json"
	catalogGatewayFeed  = "../../control-plane/fixtures/catalog/gateway-feed.json"
)

type catalogRoute struct {
	Path                string          `json:"path"`
	Method              string          `json:"method"`
	Scope               string          `json:"scope"`
	Upstream            string          `json:"upstream"`
	RequiredPermissions json.RawMessage `json:"required_permissions"`
	AllowDeleted        bool            `json:"allow_deleted"`
}

// need is the route's required_permissions as gatewayStatus takes them: nil
// outside a kind scope, "any_member", or the list.
func (r catalogRoute) need() any {
	switch raw := string(r.RequiredPermissions); raw {
	case "", "null":
		return nil
	case `"any_member"`:
		return "any_member"
	}
	var perms []string
	if err := json.Unmarshal(r.RequiredPermissions, &perms); err != nil {
		panic(err)
	}
	return perms
}

// catalog reads the identity feed, letting edit change it first, and
// identity's routes from the gateway feed.
func catalog(t *testing.T, edit func(feed map[string]any)) (string, []catalogRoute) {
	t.Helper()
	raw, err := os.ReadFile(catalogIdentityFeed)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(doc)
	}
	body, _ := json.Marshal(doc)
	raw, err = os.ReadFile(catalogGatewayFeed)
	if err != nil {
		t.Fatal(err)
	}
	var gw struct {
		Services map[string]struct {
			Routes []catalogRoute `json:"routes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(raw, &gw); err != nil {
		t.Fatal(err)
	}
	return string(body), gw.Services["identity"].Routes
}

// edgeEnv is an env with the catalog's routes and a record of which routes
// were forwarded and answered with their documented status.
type edgeEnv struct {
	*env
	routes []catalogRoute
	served map[string]bool
}

func setupCatalog(t *testing.T, edit func(feed map[string]any)) *edgeEnv {
	body, routes := catalog(t, edit)
	if len(routes) == 0 {
		t.Fatal("no identity routes in the gateway feed")
	}
	return &edgeEnv{env: setupWith(t, body), routes: routes, served: map[string]bool{}}
}

var paramRe = regexp.MustCompile(`\{([a-z_.]+)\}`)

// match finds the route for method and path, a literal segment beating a
// param, and the path's params.
func (e *edgeEnv) match(method, path string) (catalogRoute, map[string]string) {
	e.t.Helper()
	var best *catalogRoute
	var bestParams map[string]string
	for i := range e.routes {
		r := &e.routes[i]
		params, ok := r.match(method, path)
		if ok && (best == nil || r.beats(best)) {
			best, bestParams = r, params
		}
	}
	if best == nil {
		e.t.Fatalf("no route for %s %s", method, path)
	}
	return *best, bestParams
}

// match is the path's params when r serves method and path.
func (r catalogRoute) match(method, path string) (map[string]string, bool) {
	if r.Method != method {
		return nil, false
	}
	got := strings.Split(path, "/")
	want := strings.Split(r.Path, "/")
	if len(want) != len(got) {
		return nil, false
	}
	params := map[string]string{}
	for i, seg := range want {
		if m := paramRe.FindStringSubmatch(seg); m != nil && m[0] == seg {
			params[m[1]] = got[i]
		} else if seg != got[i] {
			return nil, false
		}
	}
	return params, true
}

// beats is whether r wins over o for a path both match: the first segment
// where one is a literal and the other a param.
func (r catalogRoute) beats(o *catalogRoute) bool {
	a, b := strings.Split(r.Path, "/"), strings.Split(o.Path, "/")
	for i := range a {
		la, lb := !paramRe.MatchString(a[i]), !paramRe.MatchString(b[i])
		if la != lb {
			return la
		}
	}
	return false
}

// chain is the route's kind params, root first, as the admit chain.
func (e *edgeEnv) chain(r catalogRoute, params map[string]string) []string {
	out := []string{}
	if r.Scope == "user" {
		return out
	}
	for _, m := range paramRe.FindAllStringSubmatch(r.Path, -1) {
		if _, ok := e.snap().Kind(m[1]); ok {
			out = append(out, m[1], params[m[1]])
		}
	}
	return out
}

// admitFor admits subject (an IdP subject) on route r with params.
func (e *edgeEnv) admitFor(subject string, r catalogRoute, params map[string]string) response {
	e.t.Helper()
	who := fmt.Sprintf(`"idp": {"issuer": "https://idp.test", "subject": %q}`, subject)
	return e.admit(admitBody(who, r.AllowDeleted, e.chain(r, params)...))
}

// documented is the status identity answers a route with when it succeeds.
func documented(r catalogRoute) int {
	switch {
	case r.Method == "DELETE":
		return 204
	case r.Method == "POST" && (strings.HasSuffix(r.Upstream, "/restore") || strings.HasSuffix(r.Upstream, "/redeem")):
		return 200
	case r.Method == "POST":
		return 201
	}
	return 200
}

// edge sends a request as subject through the route, as the gateway would. A
// request the gateway would refuse answers with its status and no body.
func (e *edgeEnv) edge(subject, method, target, body string) response {
	e.t.Helper()
	path, query, _ := strings.Cut(target, "?")
	r, params := e.match(method, path)
	a := e.admitFor(subject, r, params)
	if status := gatewayStatus(a, r.need()); status != 200 {
		return response{status: status}
	}
	values := map[string]string{"member.id": a.str("member_id"), "user.id": a.str("user_id")}
	for _, n := range a.list("nodes") {
		values[n["kind"].(string)+".id"] = n["id"].(string)
	}
	for k, v := range params {
		values[k] = v
	}
	upstream := paramRe.ReplaceAllStringFunc(r.Upstream, func(p string) string {
		v := values[p[1:len(p)-1]]
		if v == "" {
			e.t.Fatalf("%s %s: nothing fills %s in %s", method, r.Path, p, r.Upstream)
		}
		return v
	})
	if query != "" {
		upstream += "?" + query
	}
	var resp response
	if r.Scope == "user" {
		resp = e.callUser(method, upstream, a.str("user_id"), body)
	} else {
		resp = e.call(method, upstream, a.str("member_id"), body)
	}
	if resp.status == documented(r) {
		e.served[method+" "+r.Path] = true
	}
	return resp
}

// want sends the request and fails unless it answers status.
func (e *edgeEnv) want(status int, subject, method, target, body string) response {
	e.t.Helper()
	r := e.edge(subject, method, target, body)
	if r.status != status {
		e.t.Fatalf("%s %s as %s: status %d, want %d: %v", method, target, subject, r.status, status, r.body)
	}
	return r
}

func (e *edgeEnv) listed(subject, target, key string) []string {
	e.t.Helper()
	return ids(e.want(200, subject, "GET", target, "").list(key))
}

// walkNode calls every route on the node at path (bindings, membership,
// service accounts and keys, invites, delete and restore), as admin. other is
// a member of the tree to bind; guest redeems the node's invite, with role.
// deletedList is where the node is listed once deleted.
func (e *edgeEnv) walkNode(admin, path, other, guest, role, deletedList string) {
	e.t.Helper()
	id := e.want(200, admin, "GET", path, "").str("id")
	e.want(200, admin, "PATCH", path, `{"display_name": "Renamed"}`)
	e.want(200, admin, "GET", path+"/roles", "")
	if m := e.want(200, admin, "GET", path+"/membership", ""); !slices.Contains(permissionsOf(m), "*") {
		e.t.Fatalf("membership at %s: %v", path, m.body)
	}

	e.want(200, admin, "GET", path+"/bindings", "")
	b := e.want(201, admin, "POST", path+"/bindings", fmt.Sprintf(`{"member_id": %q, "role": %q}`, other, role)).str("id")
	e.want(200, admin, "PATCH", path+"/bindings/"+b, `{"role": "admin"}`)
	e.want(204, admin, "DELETE", path+"/bindings/"+b, "")

	sa := e.want(201, admin, "POST", path+"/service-accounts", fmt.Sprintf(`{"name": "ci", "role": %q}`, role)).str("id")
	e.want(200, admin, "GET", path+"/service-accounts", "")
	e.want(200, admin, "GET", path+"/service-accounts/"+sa, "")
	e.want(200, admin, "PATCH", path+"/service-accounts/"+sa, `{"name": "deploy"}`)
	k := e.want(201, admin, "POST", path+"/service-accounts/"+sa+"/keys", `{}`).str("id")
	e.want(200, admin, "GET", path+"/service-accounts/"+sa+"/keys", "")
	e.want(204, admin, "DELETE", path+"/service-accounts/"+sa+"/keys/"+k, "")
	e.want(204, admin, "DELETE", path+"/service-accounts/"+sa, "")

	invite := fmt.Sprintf(`{"role": %q}`, role)
	e.want(200, admin, "GET", path+"/invites", "")
	i := e.want(201, admin, "POST", path+"/invites", invite).str("id")
	e.want(204, admin, "DELETE", path+"/invites/"+i, "")
	token := e.want(201, admin, "POST", path+"/invites", invite).str("token")
	redeemed := e.want(200, guest, "POST", "/user/invites/redeem", fmt.Sprintf(`{"token": %q}`, token))
	if node, _ := redeemed.body["node"].(map[string]any); node["id"] != id {
		e.t.Fatalf("redeem at %s: %v", path, redeemed.body)
	}

	e.want(204, admin, "DELETE", path, "")
	if !slices.Contains(e.listed(admin, deletedList, "nodes"), id) {
		e.t.Fatalf("%s isn't in %s", path, deletedList)
	}
	e.want(200, admin, "POST", path+"/restore", "")
}

func TestCatalogEveryUpstreamIsServed(t *testing.T) {
	e := setupCatalog(t, nil)
	org := e.want(201, "alice", "POST", "/orgs", `{"display_name": "Gruow", "slug": "gruow"}`).str("id")
	project := e.want(201, "alice", "POST", "/orgs/gruow/projects", `{"display_name": "Gatheroom", "slug": "gatheroom"}`).str("id")
	env := e.want(201, "alice", "POST", "/orgs/gruow/projects/gatheroom/envs", `{"display_name": "Prod", "slug": "prod"}`).str("id")
	for path, want := range map[string]string{"/orgs/gruow/projects": project, "/orgs/gruow/projects/gatheroom/envs": env} {
		if got := e.listed("alice", path, "nodes"); !slices.Equal(got, []string{want}) {
			t.Fatalf("%s: %v, want %s", path, got, want)
		}
	}
	bob := e.want(201, "alice", "POST", "/orgs/gruow/members", `{"idp": {"issuer": "https://idp.test", "subject": "bob"}, "role": "member"}`).str("id")

	// Below the root, gus redeems each invite and becomes a guest; at the
	// root, the member to bind is gus, and rita redeems to join.
	e.walkNode("alice", "/orgs/gruow/projects/gatheroom/envs/prod", bob, "gus", "admin", "/orgs/gruow/projects/gatheroom/envs?status=deleted")
	e.walkNode("alice", "/orgs/gruow/projects/gatheroom", bob, "gus", "developer", "/orgs/gruow/projects?status=deleted")
	gus := e.want(200, "gus", "GET", "/orgs/gruow/projects/gatheroom/membership", "").str("member_id")
	e.walkNode("alice", "/orgs/gruow", gus, "rita", "member", "/user/nodes?kind=organization&status=deleted")

	members := e.listed("alice", "/orgs/gruow/members", "members")
	for _, m := range []string{bob, gus} {
		if !slices.Contains(members, m) {
			t.Fatalf("members %v: no %s", members, m)
		}
	}
	e.want(200, "alice", "GET", "/orgs/gruow/members/"+bob, "")
	e.want(204, "alice", "DELETE", "/orgs/gruow/members/"+bob, "")
	if got := e.want(200, "alice", "GET", "/orgs/gruow/members/"+bob, "").str("status"); got != "removed" {
		t.Fatalf("bob: %s, want removed", got)
	}
	e.want(200, "rita", "GET", "/orgs/gruow/membership", "")
	e.want(204, "rita", "DELETE", "/orgs/gruow/membership", "")
	e.want(403, "rita", "GET", "/orgs/gruow/membership", "")

	if got := e.want(200, "alice", "GET", "/user", "").str("id"); got != e.userOf("alice") {
		t.Fatalf("/user: %s", got)
	}
	if got := e.want(200, "alice", "PATCH", "/user", `{"name": "Alice A"}`).str("name"); got != "Alice A" {
		t.Fatalf("PATCH /user: %s", got)
	}
	if got := e.listed("alice", "/user/nodes?kind=environment", "nodes"); !slices.Equal(got, []string{env}) {
		t.Fatalf("/user/nodes: %v", got)
	}
	if got := e.listed("alice", "/user/nodes", "nodes"); !slices.Contains(got, org) {
		t.Fatalf("/user/nodes: %v", got)
	}
	k := e.want(201, "alice", "POST", "/user/keys", `{"name": "laptop"}`).str("id")
	e.want(200, "alice", "GET", "/user/keys", "")
	e.want(204, "alice", "DELETE", "/user/keys/"+k, "")
	e.want(204, "zed", "DELETE", "/user", "")

	for _, r := range e.routes {
		if !e.served[r.Method+" "+r.Path] {
			t.Errorf("%s %s (upstream %s) was never served with %d", r.Method, r.Path, r.Upstream, documented(r))
		}
	}
}

// roleForEachPermission adds a role "only:<permission>" for every permission
// identity's routes require, bindable at every kind, and "nobody", which
// grants nothing.
func roleForEachPermission(routes []catalogRoute) func(feed map[string]any) {
	return func(feed map[string]any) {
		roles := feed["roles"].(map[string]any)
		var kinds []any
		for k := range feed["kinds"].(map[string]any) {
			kinds = append(kinds, k)
		}
		for _, r := range routes {
			if perms, ok := r.need().([]string); ok {
				for _, p := range perms {
					roles["only:"+p] = map[string]any{"permissions": []any{p}, "bindable_at": kinds}
				}
			}
		}
		roles["nobody"] = map[string]any{"permissions": []any{}, "bindable_at": []any{"organization"}}
	}
}

// Every permission-gated route admits a member whose only grant is a role
// with one of its permissions, bound at the route's node, and refuses the
// same member without it. any_member routes admit a member with nothing.
func TestCatalogPermissionsReachTheirRoutes(t *testing.T) {
	_, routes := catalog(t, nil)
	e := setupCatalog(t, roleForEachPermission(routes))
	e.want(201, "alice", "POST", "/orgs", `{"display_name": "Gruow", "slug": "gruow"}`)
	e.want(201, "alice", "POST", "/orgs/gruow/projects", `{"display_name": "Gatheroom", "slug": "gatheroom"}`)
	e.want(201, "alice", "POST", "/orgs/gruow/projects/gatheroom/envs", `{"display_name": "Prod", "slug": "prod"}`)
	org := e.nodeAt("/orgs/gruow")
	slugs := map[string]string{"organization": "gruow", "project": "gatheroom", "environment": "prod"}

	gated, open := 0, 0
	for _, r := range e.routes {
		if r.Scope == "user" {
			continue
		}
		path := paramRe.ReplaceAllStringFunc(r.Path, func(p string) string {
			if v, ok := slugs[p[1:len(p)-1]]; ok {
				return v
			}
			return "x"
		})
		params, ok := r.match(r.Method, path)
		if !ok {
			t.Fatalf("%s %s doesn't match its own route %s", r.Method, path, r.Path)
		}
		n := gated + open
		without, with := fmt.Sprintf("without%d", n), fmt.Sprintf("with%d", n)
		e.join(org, without, "nobody")
		switch need := r.need().(type) {
		case string:
			open++
			if got := gatewayStatus(e.admitFor(without, r, params), need); got != 200 {
				t.Errorf("%s %s (any_member) for a member with nothing: %d, want 200", r.Method, r.Path, got)
			}
		case []string:
			gated++
			if got := gatewayStatus(e.admitFor(without, r, params), need); got != 403 {
				t.Errorf("%s %s with no grant: %d, want 403", r.Method, r.Path, got)
			}
			if at := e.nodeAt(scopePath(r.Path, r.Scope, slugs)); at.ID == org.ID {
				e.join(org, with, "only:"+need[0])
			} else {
				e.bind(at, e.join(org, with, "nobody"), "only:"+need[0])
			}
			if got := gatewayStatus(e.admitFor(with, r, params), need); got != 200 {
				t.Errorf("%s %s with only %s: %d, want 200", r.Method, r.Path, need[0], got)
			}
		}
	}
	if gated == 0 || open == 0 {
		t.Fatalf("permission-gated routes: %d, any_member: %d", gated, open)
	}
}

// scopePath is the public path of the route's scope node.
func scopePath(path, scope string, slugs map[string]string) string {
	end := "{" + scope + "}"
	return paramRe.ReplaceAllStringFunc(path[:strings.Index(path, end)+len(end)], func(p string) string {
		return slugs[p[1:len(p)-1]]
	})
}

// nodeAt is the node at a public path of slugs.
func (e *edgeEnv) nodeAt(path string) *nodes.Node {
	e.t.Helper()
	n, err := e.nodes.Get(e.ctx, e.want(200, "alice", "GET", path, "").str("id"))
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// The filtered lists read each kind's list_permission: what a role grants
// through it is what a member sees.
func TestCatalogListsReadTheListPermission(t *testing.T) {
	_, routes := catalog(t, nil)
	e := setupCatalog(t, roleForEachPermission(routes))
	e.want(201, "alice", "POST", "/orgs", `{"display_name": "Gruow", "slug": "gruow"}`)
	top := e.want(201, "alice", "POST", "/orgs/gruow/projects", `{"display_name": "Gatheroom", "slug": "gatheroom"}`).str("id")
	other := e.want(201, "alice", "POST", "/orgs/gruow/projects", `{"display_name": "Apps", "slug": "apps"}`).str("id")
	env := e.want(201, "alice", "POST", "/orgs/gruow/projects/apps/envs", `{"display_name": "Prod", "slug": "prod"}`).str("id")
	bob := e.want(201, "alice", "POST", "/orgs/gruow/members", `{"idp": {"issuer": "https://idp.test", "subject": "bob"}, "role": "nobody"}`).str("id")

	check := func(what, target string, want ...string) {
		t.Helper()
		got := e.listed("bob", target, "nodes")
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("%s: %s = %v, want %v", what, target, got, want)
		}
	}
	// bob's role grants nothing: the lists are empty, not a 403.
	check("no grant", "/orgs/gruow/projects")
	check("no grant", "/user/nodes?kind=project")

	bind := func(path, role string) {
		t.Helper()
		e.want(201, "alice", "POST", path+"/bindings", fmt.Sprintf(`{"member_id": %q, "role": %q}`, bob, role))
	}
	// project:read at apps lists apps, not what's below it or beside it.
	bind("/orgs/gruow/projects/apps", "only:project:read")
	check("project:read", "/orgs/gruow/projects", other)
	check("project:read", "/orgs/gruow/projects/apps/envs")
	check("project:read", "/user/nodes?kind=project", other)
	// environment:read at apps lists its environments, by inheritance.
	e.want(204, "alice", "DELETE", "/orgs/gruow/projects/apps/bindings/"+e.bindingOf(bob, other), "")
	bind("/orgs/gruow/projects/apps", "only:environment:read")
	check("environment:read", "/orgs/gruow/projects/apps/envs", env)
	check("environment:read", "/orgs/gruow/projects")

	// A permission the list doesn't read grants no place in it.
	bind("/orgs/gruow/projects/gatheroom", "only:project:write")
	check("project:write", "/orgs/gruow/projects")

	// Deleted lists read the same permission, counting the deleted node's own bindings.
	e.want(200, "alice", "PATCH", "/orgs/gruow/projects/gatheroom/bindings/"+e.bindingOf(bob, top), `{"role": "only:project:read"}`)
	e.want(204, "alice", "DELETE", "/orgs/gruow/projects/gatheroom", "")
	check("project:read, deleted", "/orgs/gruow/projects?status=deleted", top)
	check("project:read, deleted", "/user/nodes?kind=project&status=deleted", top)
	// Restoring still needs the restore route's permission.
	e.want(403, "bob", "POST", "/orgs/gruow/projects/gatheroom/restore", "")
}
