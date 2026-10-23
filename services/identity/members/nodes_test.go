package members

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plat5dev/plat5/identity/internal/auditx"
)

func TestCreateRoot(t *testing.T) {
	e := setup(t)
	dana := e.person("dana")
	before := e.lastEvent()

	r := e.call("POST", "/roots/organization", dana, `{"label": "Gruow"}`)
	e.expect(r, 201, "create org")
	orgID := r.str("id")
	if r.str("slug") != "gruow" || r.body["parent_id"] != nil {
		t.Fatalf("org %v", r.body)
	}
	var memberID string
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM members WHERE root_id = $1`, orgID).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	// The creator gets a member in the new tree with creator_role at its root.
	if n := e.count(`SELECT count(*) FROM bindings WHERE member_id = $1 AND node_id = $2 AND role = 'owner' AND expires_at IS NULL`, memberID, orgID); n != 1 {
		t.Fatalf("creator bindings %d", n)
	}
	got := e.eventsAfter(before)
	want := []string{"organization.created " + orgID + " " + orgID, "member.created " + memberID + " " + orgID + " person"}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	// A sign-in route that changes a tree names it for audit.
	if r.headers[auditx.RootHeader] != orgID || r.headers[auditx.MemberHeader] != memberID || r.headers[auditx.CreatedHeader] != orgID {
		t.Fatalf("audit headers %v", r.headers)
	}

	// With no member behind the request, no member and no binding.
	r = e.call("POST", "/roots/organization", "", `{"label": "Unowned"}`)
	e.expect(r, 201, "create org with no caller")
	if n := e.count(`SELECT count(*) FROM members WHERE root_id = $1`, r.str("id")); n != 0 {
		t.Fatalf("members %d", n)
	}
	if r.headers[auditx.RootHeader] != "" {
		t.Fatal("no member, no root header")
	}

	// The caller must be an active directory member.
	e.expect(e.call("POST", "/roots/organization", memberID, `{"label": "X"}`), 400, "org member as caller")
	e.expect(e.call("POST", "/roots/organization", "m_nope", `{"label": "X"}`), 400, "malformed caller")
	e.expect(e.call("POST", "/roots/directory", dana, `{"label": "X"}`), 422, "sign-in kind")
	e.expect(e.call("POST", "/roots/project", dana, `{"label": "X"}`), 422, "child kind")
	e.expect(e.call("POST", "/roots/organization", dana, `{"label": "Taken", "slug": "gruow"}`), 409, "slug taken")
}

func TestCreateChildBindsTheCreator(t *testing.T) {
	e := setup(t)
	org, owner := e.org(e.person("dana"), "Gruow")
	dev := e.join(org, "dev", "developer")
	until := e.clock.t.Add(48 * time.Hour)

	// An inherited owner binding with no expiry already holds creator_role.
	r := e.call("POST", "/nodes/"+org.ID+"/children/project", owner, `{"label": "Alpha"}`)
	e.expect(r, 201, "owner creates")
	alpha := r.str("id")
	if n := e.count(`SELECT count(*) FROM bindings WHERE node_id = $1`, alpha); n != 0 {
		t.Fatalf("owner got %d bindings on the project", n)
	}
	if r.headers[auditx.CreatedHeader] != alpha {
		t.Fatalf("created %q", r.headers[auditx.CreatedHeader])
	}

	// Anyone else gets creator_role on the new node.
	r = e.call("POST", "/nodes/"+org.ID+"/children/project", dev, `{"label": "Beta"}`)
	e.expect(r, 201, "developer creates")
	if n := e.count(`SELECT count(*) FROM bindings WHERE node_id = $1 AND member_id = $2 AND role = 'owner'`, r.str("id"), dev); n != 1 {
		t.Fatal("developer should be owner of what they made")
	}

	// An expiring inherited creator_role doesn't count: it would take the node with it.
	expiring := e.join(org, "expiring", "member")
	e.exec(`UPDATE bindings SET role = 'owner', expires_at = $2 WHERE member_id = $1`, expiring, until)
	r = e.call("POST", "/nodes/"+org.ID+"/children/project", expiring, `{"label": "Gamma"}`)
	e.expect(r, 201, "expiring owner creates")
	if n := e.count(`SELECT count(*) FROM bindings WHERE node_id = $1 AND member_id = $2 AND expires_at IS NULL`, r.str("id"), expiring); n != 1 {
		t.Fatal("an expiring inherited owner should still get creator_role")
	}

	// No member behind the request: no binding.
	r = e.call("POST", "/nodes/"+org.ID+"/children/project", "", `{"label": "Delta"}`)
	e.expect(r, 201, "no caller")
	if n := e.count(`SELECT count(*) FROM bindings WHERE node_id = $1`, r.str("id")); n != 0 {
		t.Fatal("no caller, no binding")
	}

	// A caller not active in this tree is malformed; nothing is created.
	_, otherOwner := e.org(e.person("erin"), "Other")
	before := e.count(`SELECT count(*) FROM nodes WHERE kind = 'project'`)
	e.expect(e.call("POST", "/nodes/"+org.ID+"/children/project", otherOwner, `{"label": "X"}`), 400, "caller from another tree")
	gone := e.join(org, "gone", "member")
	if _, err := e.store.RemoveMember(e.ctx, org.ID, gone); err != nil {
		t.Fatal(err)
	}
	e.expect(e.call("POST", "/nodes/"+org.ID+"/children/project", gone, `{"label": "X"}`), 400, "removed caller")
	if after := e.count(`SELECT count(*) FROM nodes WHERE kind = 'project'`); after != before {
		t.Fatal("a refused create must not leave a node")
	}

	e.expect(e.call("POST", "/nodes/"+org.ID+"/children/organization", owner, `{"label": "X"}`), 422, "kind not allowed here")
	e.expect(e.call("POST", "/nodes/n_01JAAAAAAAAAAAAAAAAAAAAAAA/children/project", owner, `{"label": "X"}`), 404, "missing parent")
	if _, err := e.nodes.Delete(e.ctx, e.snap(), alpha); err != nil {
		t.Fatal(err)
	}
	folder := e.child(org, "folder", "Docs")
	if _, err := e.nodes.Delete(e.ctx, e.snap(), folder.ID); err != nil {
		t.Fatal(err)
	}
	e.expect(e.call("POST", "/nodes/"+folder.ID+"/children/project", owner, `{"label": "X"}`), 409, "under a deleted node")
}

func TestFilteredChildren(t *testing.T) {
	e := setup(t)
	org, owner := e.org(e.person("dana"), "Gruow")
	p1 := e.child(org, "project", "One")
	p2 := e.child(org, "project", "Two")
	p3 := e.child(org, "project", "Three")
	dev := e.join(org, "dev", "developer")  // project:read at the org: every project
	plain := e.join(org, "plain", "member") // default-deny below the root
	e.bind(p2, plain, "viewer", nil)        // reads p2 only
	stale := e.join(org, "stale", "member") // a binding that grants nothing
	e.exec(`INSERT INTO bindings (id, member_id, node_id, role) VALUES ('b_01JAAAAAAAAAAAAAAAAAAAAAAA', $1, $2, 'gone_here')`, stale, p1.ID)
	expired := e.join(org, "expired", "member")
	e.bind(p1, expired, "viewer", ptr(e.clock.t.Add(time.Hour)))

	list := func(caller, query string) []string {
		t.Helper()
		r := e.call("GET", "/nodes/"+org.ID+"/children/project"+query, caller, "")
		e.expect(r, 200, "list "+query)
		return ids(r.list("nodes"))
	}
	all := []string{p1.ID, p2.ID, p3.ID}
	if got := list(owner, ""); !slices.Equal(got, all) {
		t.Fatalf("owner sees %v", got)
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
	// An expired binding grants nothing, even before the sweeper deletes it.
	if got := list(expired, ""); !slices.Equal(got, []string{p1.ID}) {
		t.Fatalf("before expiry %v", got)
	}
	e.clock.advance(2 * time.Hour)
	if got := list(expired, ""); len(got) != 0 {
		t.Fatalf("after expiry %v", got)
	}

	// Pages walk past unreadable nodes; a page is never empty with more to come.
	r := e.call("GET", "/nodes/"+org.ID+"/children/project?limit=1", owner, "")
	if got := ids(r.list("nodes")); !slices.Equal(got, []string{p1.ID}) || r.body["has_more"] != true {
		t.Fatalf("page 1 %v %v", got, r.body["has_more"])
	}
	r = e.call("GET", "/nodes/"+org.ID+"/children/project?limit=1&starting_after="+p1.ID, plain, "")
	if got := ids(r.list("nodes")); !slices.Equal(got, []string{p2.ID}) || r.body["has_more"] != false {
		t.Fatalf("plain after p1 %v %v", got, r.body["has_more"])
	}

	// Deleted ones need the delete route's labels; a deleted node's own bindings count.
	if _, err := e.nodes.Delete(e.ctx, e.snap(), p3.ID); err != nil {
		t.Fatal(err)
	}
	if got := list(owner, "?state=deleted"); !slices.Equal(got, []string{p3.ID}) {
		t.Fatalf("owner deleted %v", got)
	}
	if got := list(dev, "?state=deleted"); len(got) != 0 {
		t.Fatalf("developer can't delete, sees %v", got)
	}
	projOwner := e.join(org, "projowner", "member")
	e.exec(`UPDATE nodes SET status = 'active' WHERE id = $1`, p3.ID)
	e.bind(p3, projOwner, "owner", nil)
	e.exec(`UPDATE nodes SET status = 'deleted' WHERE id = $1`, p3.ID)
	if got := list(projOwner, "?state=deleted"); !slices.Equal(got, []string{p3.ID}) {
		t.Fatalf("owner of the deleted project sees %v", got)
	}
	if got := list(owner, ""); !slices.Equal(got, []string{p1.ID, p2.ID}) {
		t.Fatalf("live list after delete %v", got)
	}

	// Folders have no read route of identity's: another service's [] route doesn't count.
	e.child(org, "folder", "Docs")
	r = e.call("GET", "/nodes/"+org.ID+"/children/folder", owner, "")
	e.expect(r, 200, "folders")
	if len(r.list("nodes")) != 0 {
		t.Fatal("no read route applied: nothing is readable")
	}

	// The caller is required, and must be active in this tree.
	e.expect(e.call("GET", "/nodes/"+org.ID+"/children/project", "", ""), 400, "no caller")
	_, otherOwner := e.org(e.person("erin"), "Other")
	e.expect(e.call("GET", "/nodes/"+org.ID+"/children/project", otherOwner, ""), 400, "caller from another tree")
	e.expect(e.call("GET", "/nodes/"+org.ID+"/children/project?state=gone", owner, ""), 422, "bad state")
}

func TestOpenReadRoute(t *testing.T) {
	e := setup(t)
	// A read route with [] lets any member of the tree read.
	open := strings.Replace(testConfig, `"methods": ["GET"], "required_labels": ["project:read"]`, `"methods": ["GET"], "required_labels": []`, 1)
	e.src.Store(parseConfig(t, open))
	org, _ := e.org(e.person("dana"), "Gruow")
	p := e.child(org, "project", "One")
	plain := e.join(org, "plain", "member")
	r := e.call("GET", "/nodes/"+org.ID+"/children/project", plain, "")
	if got := ids(r.list("nodes")); !slices.Equal(got, []string{p.ID}) {
		t.Fatalf("open route: %v", got)
	}
}

func TestPersonNodes(t *testing.T) {
	e := setup(t)
	dana := e.person("dana")
	gruow, _ := e.org(dana, "Gruow")
	acme, _ := e.org(e.person("erin"), "Acme")
	g1 := e.child(gruow, "project", "G1")
	a1 := e.child(acme, "project", "A1")
	e.child(acme, "project", "A2")
	danaAcme := e.join(acme, "dana", "member")
	e.bind(a1, danaAcme, "viewer", nil)

	get := func(query string) response {
		t.Helper()
		r := e.call("GET", "/members/"+dana+"/nodes"+query, "", "")
		e.expect(r, 200, query)
		return r
	}
	r := get("?kind=project")
	if got := ids(r.list("nodes")); !slices.Equal(got, sorted(g1.ID, a1.ID)) {
		t.Fatalf("projects %v", got)
	}
	for _, n := range r.list("nodes") {
		root := map[string]string{g1.ID: "gruow", a1.ID: "acme"}[n["id"].(string)]
		if n["root_slug"] != root || n["root_label"] == "" {
			t.Fatalf("root of %v", n)
		}
	}
	if got := ids(get("?kind=organization").list("nodes")); !slices.Equal(got, sorted(gruow.ID, acme.ID)) {
		t.Fatalf("orgs %v", got)
	}
	// No kind: every kind, never the directory.
	if got := ids(get("").list("nodes")); len(got) != 4 {
		t.Fatalf("every kind %v", got)
	}

	// A deleted org is found through its own bindings; what's below it isn't listed.
	if _, err := e.nodes.Delete(e.ctx, e.snap(), gruow.ID); err != nil {
		t.Fatal(err)
	}
	if got := ids(get("?kind=organization&state=deleted").list("nodes")); !slices.Equal(got, []string{gruow.ID}) {
		t.Fatalf("deleted orgs %v", got)
	}
	if got := ids(get("?kind=project&state=deleted").list("nodes")); len(got) != 0 {
		t.Fatalf("a project under a deleted org can't be restored first: %v", got)
	}

	e.expect(e.call("GET", "/members/"+danaAcme+"/nodes", "", ""), 404, "not a directory member")
}

func TestRoles(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	p := e.child(org, "project", "One")

	r := e.call("GET", "/nodes/"+p.ID+"/roles", "", "")
	e.expect(r, 200, "project roles")
	var slugs []string
	for _, role := range r.list("roles") {
		slugs = append(slugs, role["slug"].(string))
	}
	if !slices.Equal(slugs, []string{"developer", "owner", "viewer"}) || r.body["default_role"] != nil || r.body["creator_role"] != "owner" {
		t.Fatalf("project roles %v", r.body)
	}
	r = e.call("GET", "/nodes/"+org.ID+"/roles", "", "")
	if r.body["default_role"] != "member" {
		t.Fatalf("org default %v", r.body["default_role"])
	}
	for _, role := range r.list("roles") {
		if role["slug"] == "support" && role["max_duration"] != "7d" {
			t.Fatalf("support %v", role)
		}
	}
	r = e.call("GET", "/nodes/"+e.signIn.ID+"/roles", "", "")
	if len(r.list("roles")) != 0 || r.body["default_role"] != nil || r.body["creator_role"] != nil {
		t.Fatalf("sign-in roles %v", r.body)
	}
}

func ptr[T any](v T) *T { return &v }

func sorted(s ...string) []string {
	slices.Sort(s)
	return s
}
