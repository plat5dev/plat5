package members

import (
	"fmt"
	"slices"
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
	group                string // on the org
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
	g, err := e.store.CreateGroup(e.ctx, e.snap(), tr.org.ID, "Team")
	if err != nil {
		e.t.Fatal(err)
	}
	tr.group = g.ID
	tr.sa, _ = e.serviceAccount(tr.project, "viewer")
	k, err := e.store.CreateKey(e.ctx, KeyOwner{NodeID: tr.project.ID, MemberID: tr.sa}, KeyCreate{Name: "ci"})
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
		{"create a group", "POST", o + "/groups", `{"name": "Ops"}`},
		{"rename a group", "PATCH", o + "/groups/" + tr.group, `{"name": "Renamed"}`},
		{"add a group member", "POST", o + "/groups/" + tr.group + "/members", fmt.Sprintf(`{"member_id": %q}`, tr.bob)},
		{"create a service account", "POST", p + "/service-accounts", `{"name": "deploy", "role": "viewer"}`},
		{"rename a service account", "PATCH", sa, `{"name": "renamed"}`},
		{"create a key", "POST", sa + "/keys", `{"name": "another"}`},
		{"update a key", "PATCH", sa + "/keys/" + tr.keyID, `{"name": "renamed"}`},
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
		"/nodes/" + tr.org.ID + "/groups/" + tr.group,
		"/nodes/" + tr.org.ID + "/groups/" + tr.group + "/members/" + tr.bob,
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
	want := map[string]int{"create a child": 409, "create a binding": 201, "add a member": 201, "create a group": 201, "add a group member": 201, "create a service account": 201, "create a key": 201, "create an invite": 201, "revoke a key": 204, "revoke an invite": 204}
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

func TestPurgeAfterTheGraceCleansMembersAndBindings(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("alice"), "Gruow")
	folder := e.child(org, "folder", "Web")
	other := e.child(org, "folder", "Other")

	// onlyHere has a binding only on folder; alsoOrg has bindings on folder and org.
	onlyHere := e.join(org, "only-here", "member")
	e.bind(folder, onlyHere, "browser")
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/bindings/"+e.bindingOf(onlyHere, org.ID), "", ""), 204, "drop org binding")
	alsoOrg := e.join(org, "also-org", "member")
	e.bind(folder, alsoOrg, "browser")

	saMember, _ := e.serviceAccount(folder, "browser")
	e.bind(other, saMember, "browser")
	var saPrincipal string
	if err := e.pool.QueryRow(e.ctx, `SELECT principal_id FROM members WHERE id = $1`, saMember).Scan(&saPrincipal); err != nil {
		t.Fatal(err)
	}
	e.invite(folder, "browser")

	if _, err := e.nodes.Delete(e.ctx, e.snap(), folder.ID); err != nil {
		t.Fatal(err)
	}

	// Not yet: the grace hasn't passed.
	e.clock.advance(30*24*time.Hour - time.Second)
	if n, err := e.nodes.PurgeDue(e.ctx); err != nil || n != 0 {
		t.Fatalf("early purge: %d %v", n, err)
	}

	e.clock.advance(time.Second)
	mark := e.lastEvent()
	if n, err := e.nodes.PurgeDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if e.count(`SELECT count(*) FROM bindings WHERE node_id = $1`, folder.ID) != 0 {
		t.Fatal("bindings on the purged node remain")
	}
	if e.count(`SELECT count(*) FROM invites WHERE node_id = $1`, folder.ID) != 0 {
		t.Fatal("invites on the purged node remain")
	}
	if e.count(`SELECT count(*) FROM principals WHERE id = $1`, saPrincipal) != 0 || e.count(`SELECT count(*) FROM api_keys WHERE principal_id = $1`, saPrincipal) != 0 {
		t.Fatal("the service account that belongs there remains")
	}
	if e.count(`SELECT count(*) FROM bindings WHERE member_id = $1`, saMember) != 0 {
		t.Fatal("the service account's other bindings remain")
	}
	if !e.removed(onlyHere) || e.removed(alsoOrg) || !e.removed(saMember) {
		t.Fatalf("removed: onlyHere %v alsoOrg %v sa %v", e.removed(onlyHere), e.removed(alsoOrg), e.removed(saMember))
	}
	wantRemoved := []string{
		"member.removed " + saMember + " " + org.ID + " service_account",
		"member.removed " + onlyHere + " " + org.ID + " person",
	}
	if onlyHere < saMember {
		wantRemoved[0], wantRemoved[1] = wantRemoved[1], wantRemoved[0]
	}
	want := append([]string{"node.purged folder " + folder.ID + " " + org.ID}, wantRemoved...)
	if got := e.eventsAfter(mark); !slices.Equal(got, want) {
		t.Fatalf("events:\n got  %v\n want %v", got, want)
	}

	// Purged is final: nothing more on the next sweep.
	mark = e.lastEvent()
	if n, err := e.nodes.PurgeDue(e.ctx); err != nil || n != 0 {
		t.Fatalf("second sweep: %d %v", n, err)
	}
	if got := e.eventsAfter(mark); len(got) != 0 {
		t.Fatalf("second sweep events: %v", got)
	}
}

func TestPurgingARootRemovesItsMembers(t *testing.T) {
	e := setup(t)
	alice := e.person("alice")
	org, member := e.org(alice, "Gruow")
	folder := e.child(org, "folder", "Web")
	_, elsewhere := e.org(alice, "Other")
	saMember, _ := e.serviceAccount(org, "browser")
	e.bind(folder, saMember, "browser")

	if _, err := e.nodes.Delete(e.ctx, e.snap(), org.ID); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(31 * 24 * time.Hour)

	mark := e.lastEvent()
	if n, err := e.nodes.PurgeDue(e.ctx); err != nil || n != 2 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if e.count(`SELECT count(*) FROM members WHERE root_id = $1 AND removed_at IS NULL`, org.ID) != 0 {
		t.Fatal("members of the purged root remain")
	}
	if e.count(`SELECT count(*) FROM members WHERE id = $1 AND removed_at IS NULL`, elsewhere) != 1 {
		t.Fatal("the person's member in another tree was removed")
	}
	removedEvents := []string{
		"member.removed " + saMember + " " + org.ID + " service_account",
		"member.removed " + member + " " + org.ID + " person",
	}
	if member < saMember {
		removedEvents[0], removedEvents[1] = removedEvents[1], removedEvents[0]
	}
	want := append([]string{
		"node.purged organization " + org.ID + " " + org.ID,
		"node.purged folder " + folder.ID + " " + org.ID,
	}, removedEvents...)
	if got := e.eventsAfter(mark); !slices.Equal(got, want) {
		t.Fatalf("events:\n got  %v\n want %v", got, want)
	}
}
