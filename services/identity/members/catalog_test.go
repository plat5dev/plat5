package members

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plat5dev/plat5/identity/nodes"
)

// The generated catalog (docs/reference/api/edge-catalog.md) against identity:
// every upstream in it is served, and its generated labels work. The routes
// are the control plane's own output for a kind with two parents
// (control-plane/fixtures/catalog), in the feed identity reads. Each request
// goes the way the gateway sends it (docs/concepts/access.md#how-a-request-is-decided):
// match, admit, labels, fill the upstream, forward with X-Plat5-Member-Id.

const catalogFeed = "../../control-plane/fixtures/catalog/identity-feed.json"

type catalogRoute struct {
	Path           string    `json:"path"`
	Scope          string    `json:"scope"`
	Upstream       string    `json:"upstream"`
	Methods        []string  `json:"methods"`
	RequiredLabels *[]string `json:"required_labels"`
	ResolveDeleted bool      `json:"resolve_deleted"`
}

func (r catalogRoute) labels() []string {
	if r.RequiredLabels == nil {
		return []string{}
	}
	return *r.RequiredLabels
}

// catalog reads the fixture feed, letting edit change it first, and returns
// the body and identity's routes in it.
func catalog(t *testing.T, edit func(feed map[string]any)) (string, []catalogRoute) {
	t.Helper()
	raw, err := os.ReadFile(catalogFeed)
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
	var w struct {
		Services map[string]struct {
			Routes []catalogRoute `json:"routes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatal(err)
	}
	return string(body), w.Services["identity"].Routes
}

// edgeEnv is an env with the catalog's routes and a record of which route
// methods were forwarded and succeeded.
type edgeEnv struct {
	*env
	routes []catalogRoute
	served map[string]bool
}

func setupCatalog(t *testing.T, edit func(feed map[string]any)) *edgeEnv {
	body, routes := catalog(t, edit)
	return &edgeEnv{env: setupWith(t, body), routes: routes, served: map[string]bool{}}
}

var paramRe = regexp.MustCompile(`\{([a-z_.]+)\}`)

// match finds the route for method and path, and the path's params.
func (e *edgeEnv) match(method, path string) (catalogRoute, map[string]string) {
	e.t.Helper()
	got := strings.Split(path, "/")
	for _, r := range e.routes {
		if !slices.Contains(r.Methods, method) {
			continue
		}
		want := strings.Split(r.Path, "/")
		if len(want) != len(got) {
			continue
		}
		params := map[string]string{}
		ok := true
		for i, seg := range want {
			if m := paramRe.FindStringSubmatch(seg); m != nil && m[0] == seg {
				params[m[1]] = got[i]
			} else if seg != got[i] {
				ok = false
				break
			}
		}
		if ok {
			return r, params
		}
	}
	e.t.Fatalf("no generated route for %s %s", method, path)
	return catalogRoute{}, nil
}

// admitFor admits subject (an IdP subject) on route r with params: the chain
// is the path's kind params, root first.
func (e *edgeEnv) admitFor(subject string, r catalogRoute, params map[string]string) response {
	e.t.Helper()
	chain := []string{}
	if r.Scope != e.snap().SignInKind().Name {
		for _, m := range paramRe.FindAllStringSubmatch(r.Path, -1) {
			if _, ok := e.snap().Kind(m[1]); ok {
				chain = append(chain, m[1], params[m[1]])
			}
		}
	}
	who := fmt.Sprintf(`"idp": {"issuer": "https://idp.test", "subject": %q}`, subject)
	return e.admit(admitBody(who, r.Scope, r.ResolveDeleted, chain...))
}

// edge sends a request as subject through the generated route, as the gateway
// would. A request the gateway would refuse answers with its status and no
// body.
func (e *edgeEnv) edge(subject, method, target, body string) response {
	e.t.Helper()
	path, query, _ := strings.Cut(target, "?")
	r, params := e.match(method, path)
	a := e.admitFor(subject, r, params)
	if status := gatewayStatus(a, r.labels()); status != 200 {
		return response{status: status}
	}
	ids := map[string]string{"subject.member_id": a.str("member_id")}
	for _, n := range a.list("nodes") {
		ids["subject."+n["kind"].(string)+"_id"] = n["id"].(string)
	}
	for k, v := range params {
		ids["path."+k] = v
	}
	upstream := paramRe.ReplaceAllStringFunc(r.Upstream, func(p string) string {
		v, ok := ids[p[1:len(p)-1]]
		if !ok {
			e.t.Fatalf("%s %s: nothing fills %s in %s", method, r.Path, p, r.Upstream)
		}
		return v
	})
	if query != "" {
		upstream += "?" + query
	}
	resp := e.call(method, upstream, a.str("member_id"), body)
	if resp.status < 300 {
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

func (e *edgeEnv) nodeByID(id string) *nodes.Node {
	e.t.Helper()
	n, err := e.nodes.Get(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// bindingOf is member's binding on node.
func (e *edgeEnv) bindingOf(node, member string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM bindings WHERE node_id = $1 AND member_id = $2`, node, member).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *edgeEnv) listed(subject, target, key string) []string {
	e.t.Helper()
	return ids(e.want(200, subject, "GET", target, "").list(key))
}

// walkNode calls every generated route on the node at path (bindings, service
// accounts and keys, invites, delete and restore), as owner. other is a member
// of the tree with no binding on the node; guest redeems the node's invite.
// deletedList is where the node is listed once deleted.
func (e *edgeEnv) walkNode(owner, path, other, guest, deletedList string) {
	e.t.Helper()
	id := e.want(200, owner, "GET", path, "").str("id")
	e.want(200, owner, "PATCH", path, `{"label": "Renamed"}`)
	e.want(200, owner, "GET", path+"/roles", "")
	e.want(200, owner, "GET", path+"/access", "")

	e.want(200, owner, "GET", path+"/bindings", "")
	b := e.want(201, owner, "POST", path+"/bindings", fmt.Sprintf(`{"member": %q, "role": "editor"}`, other)).str("id")
	e.want(200, owner, "PATCH", path+"/bindings/"+b, `{"role": "owner"}`)
	e.want(204, owner, "DELETE", path+"/bindings/"+b, "")

	sa := e.want(201, owner, "POST", path+"/service-accounts", `{"name": "ci", "role": "editor"}`).str("id")
	e.want(200, owner, "GET", path+"/service-accounts", "")
	e.want(200, owner, "GET", path+"/service-accounts/"+sa, "")
	e.want(200, owner, "PATCH", path+"/service-accounts/"+sa, `{"name": "deploy"}`)
	k := e.want(201, owner, "POST", path+"/service-accounts/"+sa+"/keys", `{}`).str("id")
	e.want(200, owner, "GET", path+"/service-accounts/"+sa+"/keys", "")
	e.want(204, owner, "DELETE", path+"/service-accounts/"+sa+"/keys/"+k, "")
	e.want(204, owner, "DELETE", path+"/service-accounts/"+sa, "")

	invite := fmt.Sprintf(`{"role": "editor", "expires_at": %q}`, rfc(e.clock.t.Add(24*time.Hour)))
	e.want(200, owner, "GET", path+"/invites", "")
	i := e.want(201, owner, "POST", path+"/invites", invite).str("id")
	e.want(204, owner, "DELETE", path+"/invites/"+i, "")
	token := e.want(201, owner, "POST", path+"/invites", invite).str("token")
	redeemed := e.want(200, guest, "POST", "/me/invites/redeem", fmt.Sprintf(`{"token": %q}`, token))
	if redeemed.str("node_id") != id {
		e.t.Fatalf("redeem at %s: %v", path, redeemed.body)
	}

	e.want(204, owner, "DELETE", path, "")
	if !slices.Contains(e.listed(owner, deletedList, "nodes"), id) {
		e.t.Fatalf("%s isn't in %s", path, deletedList)
	}
	e.want(200, owner, "POST", path+"/restore", "")
}

func TestCatalogEveryUpstreamIsServed(t *testing.T) {
	e := setupCatalog(t, nil)
	org := e.want(201, "alice", "POST", "/orgs", `{"label": "Gruow", "slug": "gruow"}`).str("id")
	folder := e.want(201, "alice", "POST", "/orgs/gruow/folders", `{"label": "Eng", "slug": "eng"}`).str("id")
	top := e.want(201, "alice", "POST", "/orgs/gruow/projects", `{"label": "Gatheroom", "slug": "gatheroom"}`).str("id")
	nested := e.want(201, "alice", "POST", "/orgs/gruow/folders/eng/projects", `{"label": "Api", "slug": "api"}`).str("id")
	for path, want := range map[string]string{"/orgs/gruow/folders": folder, "/orgs/gruow/projects": top, "/orgs/gruow/folders/eng/projects": nested} {
		if got := e.listed("alice", path, "nodes"); !slices.Equal(got, []string{want}) {
			t.Fatalf("%s: %v, want %s", path, got, want)
		}
	}
	bob := e.want(201, "alice", "POST", "/orgs/gruow/members", `{"idp": {"issuer": "https://idp.test", "subject": "bob"}}`).str("id")

	// Below the root, gus redeems each invite and becomes a guest; at the
	// root, the member to bind is gus, and rita redeems to join.
	e.walkNode("alice", "/orgs/gruow/projects/gatheroom", bob, "gus", "/orgs/gruow/projects?state=deleted")
	e.walkNode("alice", "/orgs/gruow/folders/eng/projects/api", bob, "gus", "/orgs/gruow/folders/eng/projects?state=deleted")
	e.walkNode("alice", "/orgs/gruow/folders/eng", bob, "gus", "/orgs/gruow/folders?state=deleted")
	gus := e.admitFor("gus", catalogRoute{Scope: "organization", Path: "/orgs/{organization}"}, map[string]string{"organization": "gruow"}).str("member_id")
	e.walkNode("alice", "/orgs/gruow", gus, "rita", "/me/nodes?kind=organization&state=deleted")

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
	e.want(204, "rita", "DELETE", "/orgs/gruow/me", "")

	e.want(200, "alice", "GET", "/me", "")
	if got := e.listed("alice", "/me/nodes?kind=project", "nodes"); !slices.Equal(got, []string{top, nested}) && !slices.Equal(got, []string{nested, top}) {
		t.Fatalf("/me/nodes: %v", got)
	}
	if got := e.listed("alice", "/me/nodes", "nodes"); !slices.Contains(got, org) {
		t.Fatalf("/me/nodes: %v", got)
	}
	k := e.want(201, "alice", "POST", "/me/keys", `{"name": "laptop"}`).str("id")
	e.want(200, "alice", "GET", "/me/keys", "")
	e.want(204, "alice", "DELETE", "/me/keys/"+k, "")
	e.want(204, "zed", "DELETE", "/me", "")

	for _, r := range e.routes {
		for _, m := range r.Methods {
			if !e.served[m+" "+r.Path] {
				t.Errorf("%s %s (upstream %s) was never served", m, r.Path, r.Upstream)
			}
		}
	}
}

// roleForEachLabel adds a role "only:<label>" for every label the catalog
// requires, bindable at every kind but the sign-in kind, and "nobody", which
// grants nothing, as the default role.
func roleForEachLabel(feed map[string]any) {
	roles := feed["roles"].(map[string]any)
	bindable := []string{"organization", "folder", "project"}
	for _, svc := range feed["services"].(map[string]any) {
		for _, r := range svc.(map[string]any)["routes"].([]any) {
			ls, _ := r.(map[string]any)["required_labels"].([]any)
			for _, l := range ls {
				roles["only:"+l.(string)] = map[string]any{"labels": []any{l}, "bindable_at": bindable}
			}
		}
	}
	roles["nobody"] = map[string]any{"labels": []any{}, "bindable_at": []any{"organization"}}
	feed["default_role"] = "nobody"
}

// Every label-gated route in the catalog admits a member whose only grant is
// a role with that label, bound where edge-catalog.md#generated-labels says
// it's checked, and refuses the same member without it.
func TestCatalogGeneratedLabelsReachTheirRoutes(t *testing.T) {
	e := setupCatalog(t, roleForEachLabel)
	e.want(201, "alice", "POST", "/orgs", `{"label": "Gruow", "slug": "gruow"}`)
	e.want(201, "alice", "POST", "/orgs/gruow/folders", `{"label": "Eng", "slug": "eng"}`)
	e.want(201, "alice", "POST", "/orgs/gruow/projects", `{"label": "Gatheroom", "slug": "gatheroom"}`)
	e.want(201, "alice", "POST", "/orgs/gruow/folders/eng/projects", `{"label": "Api", "slug": "api"}`)
	org := e.want(200, "alice", "GET", "/orgs/gruow", "").str("id")

	n := 0
	for _, r := range e.routes {
		if len(r.labels()) == 0 {
			continue
		}
		label := r.labels()[0]
		// This chain's slugs in the path, and the node the label is checked
		// at: the route's scope node, or the root for guests.
		slugs := map[string]string{"organization": "gruow", "folder": "eng", "project": "gatheroom"}
		if strings.Contains(r.Path, "{folder}") {
			slugs["project"] = "api"
		}
		fill := func(tmpl string) string {
			return paramRe.ReplaceAllStringFunc(tmpl, func(p string) string {
				if v, ok := slugs[p[1:len(p)-1]]; ok {
					return v
				}
				return "x"
			})
		}
		path := fill(r.Path)
		scope := "{" + r.Scope + "}"
		at := e.want(200, "alice", "GET", fill(r.Path[:strings.Index(r.Path, scope)+len(scope)]), "").str("id")
		if strings.HasSuffix(label, ":guests:write") {
			at = org
		}
		for _, method := range r.Methods {
			n++
			_, params := e.match(method, path)
			without, with := fmt.Sprintf("without%d", n), fmt.Sprintf("with%d", n)
			e.join(e.nodeByID(org), without, "nobody")
			if got := gatewayStatus(e.admitFor(without, r, params), r.labels()); got != 403 {
				t.Errorf("%s %s with no grant: %d, want 403", method, r.Path, got)
			}
			if at == org {
				e.join(e.nodeByID(org), with, "only:"+label)
			} else {
				e.bind(e.nodeByID(at), e.join(e.nodeByID(org), with, "nobody"), "only:"+label, nil)
			}
			if got := gatewayStatus(e.admitFor(with, r, params), r.labels()); got != 200 {
				t.Errorf("%s %s with only %s: %d, want 200", method, r.Path, label, got)
			}
		}
	}
	if n == 0 {
		t.Fatal("no label-gated routes")
	}
}

// The filtered lists read the generated read and delete labels: what a role
// grants through them is what a member sees.
func TestCatalogListsReadTheGeneratedLabels(t *testing.T) {
	e := setupCatalog(t, roleForEachLabel)
	e.want(201, "alice", "POST", "/orgs", `{"label": "Gruow", "slug": "gruow"}`)
	folder := e.want(201, "alice", "POST", "/orgs/gruow/folders", `{"label": "Eng", "slug": "eng"}`).str("id")
	top := e.want(201, "alice", "POST", "/orgs/gruow/projects", `{"label": "Gatheroom", "slug": "gatheroom"}`).str("id")
	nested := e.want(201, "alice", "POST", "/orgs/gruow/folders/eng/projects", `{"label": "Api", "slug": "api"}`).str("id")
	bob := e.want(201, "alice", "POST", "/orgs/gruow/members", `{"idp": {"issuer": "https://idp.test", "subject": "bob"}}`).str("id")

	check := func(what, target string, want ...string) {
		t.Helper()
		got := e.listed("bob", target, "nodes")
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("%s: %s = %v, want %v", what, target, got, want)
		}
	}
	// bob's default role grants nothing: the lists are empty, not a 403.
	check("no grant", "/orgs/gruow/folders")
	check("no grant", "/orgs/gruow/projects")
	check("no grant", "/me/nodes?kind=project")

	// folder:read at the folder lists the folder; project:read there lists the
	// projects under it, by inheritance, and no others.
	bind := func(path, role string) {
		t.Helper()
		e.want(201, "alice", "POST", path+"/bindings", fmt.Sprintf(`{"member": %q, "role": %q}`, bob, role))
	}
	bind("/orgs/gruow/folders/eng", "only:folder:read")
	check("folder:read", "/orgs/gruow/folders", folder)
	check("folder:read", "/orgs/gruow/folders/eng/projects")
	e.want(204, "alice", "DELETE", "/orgs/gruow/folders/eng/bindings/"+e.bindingOf(folder, bob), "")
	bind("/orgs/gruow/folders/eng", "editor")
	check("editor", "/orgs/gruow/folders", folder)
	check("editor", "/orgs/gruow/folders/eng/projects", nested)
	check("editor", "/orgs/gruow/projects")
	check("editor", "/me/nodes?kind=project", nested)

	// A label the list doesn't read grants no place in it.
	bind("/orgs/gruow/projects/gatheroom", "only:project:write")
	check("project:write", "/orgs/gruow/projects")

	// Deleted lists read the delete label.
	e.want(200, "alice", "PATCH", "/orgs/gruow/projects/gatheroom/bindings/"+e.bindingOf(top, bob), `{"role": "only:project:delete"}`)
	check("project:delete", "/orgs/gruow/projects")
	e.want(204, "alice", "DELETE", "/orgs/gruow/projects/gatheroom", "")
	e.want(204, "alice", "DELETE", "/orgs/gruow/folders/eng/projects/api", "")
	check("editor, deleted", "/orgs/gruow/folders/eng/projects?state=deleted")
	check("project:delete, deleted", "/orgs/gruow/projects?state=deleted", top)
	check("project:delete, deleted", "/me/nodes?kind=project&state=deleted", top)
	// The same label reaches the restore route.
	e.want(200, "bob", "POST", "/orgs/gruow/projects/gatheroom/restore", "")
	check("restored, project:delete only", "/orgs/gruow/projects")
}
