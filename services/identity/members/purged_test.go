package members

import (
	"fmt"
	"testing"
	"time"

	"github.com/plat5dev/plat5/identity/nodes"
)

// A purged node is read-only: every write to it or to what hangs off it is
// 409 NODE_PURGED, except a slug release and the deletes that find nothing
// left. A deleted node not yet purged takes them all
// (docs/concepts/lifecycle.md#deleting-restoring-and-purging).

// purgedTree is an org > folder > project with something of each kind hanging
// off the project and the org.
type purgedTree struct {
	org, folder, project *nodes.Node
	admin, bob           string // members of the org
	binding              string // bob's on the project
	sa, keyID            string // a service account on the project, and its key
	invite               string // on the project
}

func newPurgedTree(e *env) purgedTree {
	e.t.Helper()
	var tr purgedTree
	tr.org, tr.admin = e.org(e.person("alice"), "Gruow")
	tr.folder = e.child(tr.org, "folder", "Web")
	tr.project = e.child(tr.folder, "project", "Site")
	tr.bob = e.join(tr.org, "bob", "member")
	tr.binding = e.bind(tr.project, tr.bob, "developer").ID
	tr.sa, _ = e.serviceAccount(tr.project, "viewer")
	k, err := e.store.CreateKey(e.ctx, KeyOwner{NodeID: tr.project.ID, MemberID: tr.sa}, "ci")
	if err != nil {
		e.t.Fatal(err)
	}
	tr.keyID = k.ID
	tr.invite = e.invite(tr.project, "viewer").ID
	return tr
}

// purgedWrite is one write on the tree, and the path it goes to.
type purgedWrite struct {
	name, method, path, body string
}

func (tr purgedTree) writes() []purgedWrite {
	p, o := "/nodes/"+tr.project.ID, "/nodes/"+tr.org.ID
	sa := p + "/service-accounts/" + tr.sa
	return []purgedWrite{
		{"rename", "PATCH", p, `{"display_name": "Renamed"}`},
		{"create a child", "POST", "/nodes/" + tr.folder.ID + "/children/project", `{"display_name": "X"}`},
		{"create a binding", "POST", p + "/bindings", fmt.Sprintf(`{"member_id": %q, "role": "viewer"}`, tr.admin)},
		{"change a binding", "PATCH", p + "/bindings/" + tr.binding, `{"role": "viewer"}`},
		{"add a member", "POST", o + "/members", `{"idp": {"issuer": "https://idp.test", "subject": "carol"}, "role": "member"}`},
		{"create a service account", "POST", p + "/service-accounts", `{"name": "deploy", "role": "viewer"}`},
		{"rename a service account", "PATCH", sa, `{"name": "renamed"}`},
		{"create a key", "POST", sa + "/keys", `{"name": "another"}`},
		{"revoke a key", "DELETE", sa + "/keys/" + tr.keyID, ""},
		{"create an invite", "POST", p + "/invites", `{"role": "viewer"}`},
		{"revoke an invite", "DELETE", p + "/invites/" + tr.invite, ""},
	}
}

func TestPurgedNodeIsReadOnly(t *testing.T) {
	e := setup(t)
	tr := newPurgedTree(e)
	if _, err := e.nodes.Delete(e.ctx, e.snap(), tr.org.ID); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(31 * 24 * time.Hour)
	if n, err := e.nodes.PurgeDue(e.ctx); err != nil || n != 3 {
		t.Fatalf("purge: %d %v", n, err)
	}

	for _, w := range tr.writes() {
		r := e.call(w.method, w.path, "", w.body)
		if r.status != 409 || r.code() != "NODE_PURGED" || r.details() != nil {
			t.Fatalf("%s: %d %v", w.name, r.status, r.body)
		}
	}
	r := e.call("PATCH", "/nodes/"+tr.project.ID, "", `{"display_name": "Renamed"}`)
	if r.message() != "This project was permanently deleted." {
		t.Fatalf("message %q", r.message())
	}
	r = e.call("POST", "/nodes/"+tr.org.ID+"/members", "", `{"idp": {"issuer": "https://idp.test", "subject": "carol"}, "role": "member"}`)
	if r.message() != "This organization was permanently deleted." {
		t.Fatalf("message %q", r.message())
	}
	if n := e.count(`SELECT count(*) FROM principals WHERE kind = 'person'`); n != 2 {
		t.Fatalf("a refused add created a person: %d", n)
	}
	if got := e.nodeName(tr.project.ID); got != "Site" {
		t.Fatalf("a refused rename changed the node: %q", got)
	}

	// The deletes that find nothing left still answer 204.
	for _, path := range []string{
		"/nodes/" + tr.project.ID,
		"/nodes/" + tr.project.ID + "/bindings/" + tr.binding,
		"/nodes/" + tr.org.ID + "/members/" + tr.bob,
		"/nodes/" + tr.org.ID + "/memberships/" + tr.bob,
		"/nodes/" + tr.project.ID + "/service-accounts/" + tr.sa,
	} {
		e.expect(e.call("DELETE", path, "", ""), 204, "delete "+path)
	}
	// A restore past the grace is GRACE_EXPIRED, purged or not.
	if r := e.call("POST", "/nodes/"+tr.project.ID+"/restore", "", ""); r.status != 409 || r.code() != "GRACE_EXPIRED" {
		t.Fatalf("restore: %d %v", r.status, r.body)
	}
	// The purged root's slug can still be released.
	r = e.call("POST", "/nodes/"+tr.org.ID+"/release-slug", "", "")
	e.expect(r, 200, "release slug")
	if r.body["slug"] != nil {
		t.Fatalf("release: %v", r.body)
	}
}

// A deleted node, not yet purged, takes every write it took before: it can
// still be restored. Only a child create under it is refused, PARENT_DELETED.
func TestDeletedNodeTakesWrites(t *testing.T) {
	e := setup(t)
	tr := newPurgedTree(e)
	if _, err := e.nodes.Delete(e.ctx, e.snap(), tr.org.ID); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"create a child": 409, "create a binding": 201, "add a member": 201, "create a service account": 201, "create a key": 201, "create an invite": 201, "revoke a key": 204, "revoke an invite": 204}
	for _, w := range tr.writes() {
		status, ok := want[w.name]
		if !ok {
			status = 200
		}
		r := e.call(w.method, w.path, "", w.body)
		if r.status != status {
			t.Fatalf("%s: %d %v", w.name, r.status, r.body)
		}
		if w.name == "create a child" && r.code() != "PARENT_DELETED" {
			t.Fatalf("%s: %v", w.name, r.body)
		}
	}
}

func (e *env) nodeName(nodeID string) string {
	e.t.Helper()
	var name string
	if err := e.pool.QueryRow(e.ctx, `SELECT display_name FROM nodes WHERE id = $1`, nodeID).Scan(&name); err != nil {
		e.t.Fatal(err)
	}
	return name
}
