package members

import (
	"slices"
	"testing"

	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/id"
)

func TestCreateRoot(t *testing.T) {
	e := setup(t)
	dana := e.person("dana")
	before := e.lastEvent()

	r := e.callUser("POST", "/roots/organization", dana, `{"display_name": "Gruow"}`)
	e.expect(r, 201, "create org")
	orgID := r.str("id")
	if r.str("slug") != "gruow" || r.body["parent_id"] != nil || r.str("display_name") != "Gruow" || len(r.list("ancestors")) != 0 || r.body["purge_after"] != nil {
		t.Fatalf("org %v", r.body)
	}
	var memberID string
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM members WHERE root_id = $1`, orgID).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	// The user gets a member in the new tree with creator_role at its root.
	if n := e.count(`SELECT count(*) FROM bindings WHERE member_id = $1 AND node_id = $2 AND role = 'admin'`, memberID, orgID); n != 1 {
		t.Fatalf("creator bindings %d", n)
	}
	got := e.eventsAfter(before)
	want := []string{"node.created organization " + orgID + " " + orgID, "member.created " + memberID + " " + orgID + " person"}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	// A user route that changes a tree names it for audit.
	if r.headers[auditx.NodesHeader] != orgID || r.headers[auditx.MemberHeader] != memberID || r.headers[auditx.CreatedHeader] != orgID {
		t.Fatalf("audit headers %v", r.headers)
	}

	// With no user behind the request, no member and no binding.
	r = e.call("POST", "/roots/organization", "", `{"display_name": "Unowned"}`)
	e.expect(r, 201, "create org with no caller")
	if n := e.count(`SELECT count(*) FROM members WHERE root_id = $1`, r.str("id")); n != 0 {
		t.Fatalf("members %d", n)
	}
	if r.headers[auditx.NodesHeader] != "" {
		t.Fatal("no member, no named nodes")
	}

	// The user must be an active user; a member id isn't one.
	e.expect(e.callUser("POST", "/roots/organization", memberID, `{"display_name": "X"}`), 400, "a member id as the user")
	e.expect(e.callUser("POST", "/roots/organization", "nope", `{"display_name": "X"}`), 400, "malformed user")
	suspended := e.person("sus")
	if _, _, err := e.store.SetStatus(e.ctx, suspended, StatusSuspended); err != nil {
		t.Fatal(err)
	}
	e.expect(e.callUser("POST", "/roots/organization", suspended, `{"display_name": "X"}`), 400, "suspended user")
	// X-Plat5-Member-Id is ignored here.
	e.expect(e.call("POST", "/roots/organization", "nope", `{"display_name": "Ignored"}`), 201, "member header ignored")
	e.expect(e.callUser("POST", "/roots/folder", dana, `{"display_name": "X"}`), 422, "child kind")
	e.expect(e.callUser("POST", "/roots/team", dana, `{"display_name": "X"}`), 422, "kind not in the config")
	e.expect(e.callUser("POST", "/roots/organization", dana, `{"display_name": "Taken", "slug": "gruow"}`), 409, "slug taken")
	e.expect(e.callUser("POST", "/roots/organization", dana, `{"label": "Old field"}`), 422, "display_name required")
}

func TestCreateChildBindsTheCreator(t *testing.T) {
	e := setup(t)
	org, admin := e.org(e.person("dana"), "Gruow")
	dev := e.join(org, "dev", "developer")

	// The creator always gets creator_role, even holding it above.
	r := e.call("POST", "/nodes/"+org.ID+"/children/folder", admin, `{"display_name": "Alpha"}`)
	e.expect(r, 201, "admin creates")
	alpha := r.str("id")
	if n := e.count(`SELECT count(*) FROM bindings WHERE node_id = $1 AND member_id = $2 AND role = 'admin'`, alpha, admin); n != 1 {
		t.Fatalf("admin got %d bindings on the folder", n)
	}
	if r.headers[auditx.CreatedHeader] != alpha || len(r.list("ancestors")) != 1 {
		t.Fatalf("created %q, %v", r.headers[auditx.CreatedHeader], r.body)
	}
	r = e.call("POST", "/nodes/"+org.ID+"/children/folder", dev, `{"display_name": "Beta"}`)
	e.expect(r, 201, "developer creates")
	if n := e.count(`SELECT count(*) FROM bindings WHERE node_id = $1 AND member_id = $2 AND role = 'admin'`, r.str("id"), dev); n != 1 {
		t.Fatal("developer should be admin of what they made")
	}

	// No member behind the request: no binding.
	r = e.call("POST", "/nodes/"+org.ID+"/children/folder", "", `{"display_name": "Delta"}`)
	e.expect(r, 201, "no caller")
	if n := e.count(`SELECT count(*) FROM bindings WHERE node_id = $1`, r.str("id")); n != 0 {
		t.Fatal("no caller, no binding")
	}

	// A caller not active in this tree is malformed; nothing is created.
	_, otherAdmin := e.org(e.person("erin"), "Other")
	before := e.count(`SELECT count(*) FROM nodes WHERE kind = 'folder'`)
	e.expect(e.call("POST", "/nodes/"+org.ID+"/children/folder", otherAdmin, `{"display_name": "X"}`), 400, "caller from another tree")
	gone := e.join(org, "gone", "member")
	if _, err := e.store.RemoveMember(e.ctx, org.ID, gone); err != nil {
		t.Fatal(err)
	}
	e.expect(e.call("POST", "/nodes/"+org.ID+"/children/folder", gone, `{"display_name": "X"}`), 400, "removed caller")
	if after := e.count(`SELECT count(*) FROM nodes WHERE kind = 'folder'`); after != before {
		t.Fatal("a refused create must not leave a node")
	}

	r = e.call("POST", "/nodes/"+org.ID+"/children/project", admin, `{"display_name": "X"}`)
	if r.status != 422 || r.message() != "Kind project can't go here." {
		t.Fatalf("kind not allowed here: %d %v", r.status, r.body)
	}
	e.expect(e.call("POST", "/nodes/"+id.New()+"/children/folder", admin, `{"display_name": "X"}`), 404, "missing parent")
	folder := e.child(org, "folder", "Docs")
	if _, err := e.nodes.Delete(e.ctx, e.snap(), folder.ID); err != nil {
		t.Fatal(err)
	}
	e.expect(e.call("POST", "/nodes/"+folder.ID+"/children/project", admin, `{"display_name": "X"}`), 409, "under a deleted node")
}

func TestChildLists(t *testing.T) {
	e := setup(t)
	org, admin := e.org(e.person("dana"), "Gruow")
	apps := e.child(org, "folder", "Apps")
	p1 := e.child(apps, "project", "One")
	p2 := e.child(apps, "project", "Two")
	p3 := e.child(apps, "project", "Three")
	dev := e.join(org, "dev", "developer")      // project:read at the org: every project
	plain := e.join(org, "plain", "member")     // default-deny below the root
	e.bind(p2, plain, "viewer")                 // reads p2 only
	stale := e.join(org, "stale", "member")     // a binding that grants nothing
	browser := e.join(org, "browse", "browser") // folder:read, no project:read
	e.exec(`INSERT INTO bindings (id, member_id, node_id, role) VALUES ($1, $2, $3, 'gone_here')`, id.New(), stale, p1.ID)

	list := func(caller, query string) []string {
		t.Helper()
		r := e.call("GET", "/nodes/"+apps.ID+"/children/project"+query, caller, "")
		e.expect(r, 200, "list "+query)
		return ids(r.list("nodes"))
	}
	all := []string{p1.ID, p2.ID, p3.ID}
	if got := list(admin, ""); !slices.Equal(got, all) {
		t.Fatalf("admin sees %v", got)
	}
	if got := list(dev, ""); !slices.Equal(got, all) {
		t.Fatalf("developer sees %v", got)
	}
	if got := list(plain, ""); !slices.Equal(got, []string{p2.ID}) {
		t.Fatalf("plain member sees %v", got)
	}
	// A role not bindable where it sits grants nothing.
	if got := list(stale, ""); len(got) != 0 {
		t.Fatalf("stale binding sees %v", got)
	}
	// The kind's list_permission decides, not some other permission.
	if got := list(browser, ""); len(got) != 0 {
		t.Fatalf("folder:read sees projects %v", got)
	}
	r := e.call("GET", "/nodes/"+org.ID+"/children/folder", browser, "")
	if got := ids(r.list("nodes")); !slices.Equal(got, []string{apps.ID}) {
		t.Fatalf("folder:read sees folders %v", got)
	}
	// Without a member header, the full list: any front door can list.
	if got := list("", ""); !slices.Equal(got, all) {
		t.Fatalf("no caller sees %v", got)
	}

	// Pages walk past unseen nodes; a page is never empty with more to come.
	r = e.call("GET", "/nodes/"+apps.ID+"/children/project?limit=1", admin, "")
	if got := ids(r.list("nodes")); !slices.Equal(got, []string{p1.ID}) || r.body["has_more"] != true {
		t.Fatalf("page 1 %v %v", got, r.body["has_more"])
	}
	r = e.call("GET", "/nodes/"+apps.ID+"/children/project?limit=1&starting_after="+p1.ID, plain, "")
	if got := ids(r.list("nodes")); !slices.Equal(got, []string{p2.ID}) || r.body["has_more"] != false {
		t.Fatalf("plain after p1 %v %v", got, r.body["has_more"])
	}

	// Deleted ones use the same list_permission; a deleted node's own bindings count.
	if _, err := e.nodes.Delete(e.ctx, e.snap(), p3.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.nodes.Delete(e.ctx, e.snap(), p2.ID); err != nil {
		t.Fatal(err)
	}
	if got := list(dev, "?status=deleted"); !slices.Equal(got, []string{p2.ID, p3.ID}) {
		t.Fatalf("developer deleted %v", got)
	}
	if got := list(plain, "?status=deleted"); !slices.Equal(got, []string{p2.ID}) {
		t.Fatalf("viewer of the deleted project sees %v", got)
	}
	if got := list(admin, ""); !slices.Equal(got, []string{p1.ID}) {
		t.Fatalf("live list after delete %v", got)
	}
	r = e.call("GET", "/nodes/"+apps.ID+"/children/project", admin, "")
	if n := r.list("nodes")[0]; len(n["ancestors"].([]any)) != 2 {
		t.Fatalf("listed nodes carry ancestors: %v", n)
	}

	// A caller, when sent, must be active in this tree.
	_, otherAdmin := e.org(e.person("erin"), "Other")
	e.expect(e.call("GET", "/nodes/"+apps.ID+"/children/project", otherAdmin, ""), 400, "caller from another tree")
	e.expect(e.call("GET", "/nodes/"+apps.ID+"/children/project", "nope", ""), 400, "malformed caller")
	e.expect(e.call("GET", "/nodes/"+apps.ID+"/children/project?status=gone", admin, ""), 422, "bad status")
}

func TestUserNodes(t *testing.T) {
	e := setup(t)
	dana := e.person("dana")
	gruow, _ := e.org(dana, "Gruow")
	acme, _ := e.org(e.person("erin"), "Acme")
	gApps := e.child(gruow, "folder", "Apps")
	g1 := e.child(gApps, "project", "G1")
	aApps := e.child(acme, "folder", "Apps")
	a1 := e.child(aApps, "project", "A1")
	e.child(aApps, "project", "A2")
	danaAcme := e.join(acme, "dana", "member")
	e.bind(a1, danaAcme, "viewer")

	get := func(query string) response {
		t.Helper()
		r := e.call("GET", "/users/"+dana+"/nodes"+query, "", "")
		e.expect(r, 200, query)
		return r
	}
	r := get("?kind=project")
	if got := ids(r.list("nodes")); !slices.Equal(got, sorted(g1.ID, a1.ID)) {
		t.Fatalf("projects %v", got)
	}
	for _, n := range r.list("nodes") {
		root := map[string]string{g1.ID: "gruow", a1.ID: "acme"}[n["id"].(string)]
		ancestors := n["ancestors"].([]any)
		if len(ancestors) != 2 || ancestors[0].(map[string]any)["slug"] != root || ancestors[1].(map[string]any)["kind"] != "folder" {
			t.Fatalf("ancestors of %v", n)
		}
		if _, ok := n["root_slug"]; ok {
			t.Fatal("ancestors replace root_slug")
		}
	}
	if got := ids(get("?kind=organization").list("nodes")); !slices.Equal(got, sorted(gruow.ID, acme.ID)) {
		t.Fatalf("orgs %v", got)
	}
	// No kind: every kind dana can see. In acme, only the org (member) and a1.
	if got := ids(get("").list("nodes")); len(got) != 5 {
		t.Fatalf("every kind %v", got)
	}

	// A deleted org is found through its own bindings; what's below it isn't listed.
	if _, err := e.nodes.Delete(e.ctx, e.snap(), gruow.ID); err != nil {
		t.Fatal(err)
	}
	if got := ids(get("?kind=organization&status=deleted").list("nodes")); !slices.Equal(got, []string{gruow.ID}) {
		t.Fatalf("deleted orgs %v", got)
	}
	if got := ids(get("?kind=project&status=deleted").list("nodes")); len(got) != 0 {
		t.Fatalf("a project under a deleted org can't be restored first: %v", got)
	}

	e.expect(e.call("GET", "/users/"+danaAcme+"/nodes", "", ""), 404, "a member id isn't a user")
}

func TestRoles(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	f := e.child(org, "folder", "One")
	p := e.child(f, "project", "One")

	r := e.call("GET", "/nodes/"+p.ID+"/roles", "", "")
	e.expect(r, 200, "project roles")
	got := map[string][]string{}
	var slugs []string
	for _, role := range r.list("roles") {
		slug := role["slug"].(string)
		slugs = append(slugs, slug)
		for _, perm := range role["permissions"].([]any) {
			got[slug] = append(got[slug], perm.(string))
		}
	}
	if !slices.Equal(slugs, []string{"admin", "developer", "viewer"}) || r.body["creator_role"] != "admin" {
		t.Fatalf("project roles %v", r.body)
	}
	// Includes resolved, sorted; * as written.
	if !slices.Equal(got["developer"], []string{"organization:read", "project:read", "project:write"}) || !slices.Equal(got["admin"], []string{"*"}) {
		t.Fatalf("permissions %v", got)
	}
	r = e.call("GET", "/nodes/"+org.ID+"/roles", "", "")
	if len(r.list("roles")) != 6 || r.body["creator_role"] != "admin" {
		t.Fatalf("org roles %v", r.body)
	}
	if _, ok := r.body["default_role"]; ok {
		t.Fatal("no default_role")
	}
}

func sorted(s ...string) []string {
	slices.Sort(s)
	return s
}
