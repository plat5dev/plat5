package members

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/id"
)

func TestGroupsAndGroupBindings(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("alice"), "Gruow")
	f := e.child(org, "folder", "Apps")
	p1 := e.child(f, "project", "Web")
	p2 := e.child(f, "project", "Api")
	other, outsider := e.org(e.person("erin"), "Acme")

	// Groups exist only on roots.
	e.expect(e.call("POST", "/nodes/"+f.ID+"/groups", "", `{"name": "Backend"}`), 404, "group below root")
	e.expect(e.call("GET", "/nodes/"+f.ID+"/groups", "", ""), 404, "list below root")
	e.expect(e.call("POST", "/nodes/"+org.ID+"/groups", "", `{"name": "   "}`), 422, "empty group name")

	// Create, get, list, rename.
	r := e.call("POST", "/nodes/"+org.ID+"/groups", "", `{"name": " Backend "}`)
	e.expect(r, 201, "create group")
	g1 := r.str("id")
	if r.str("name") != "Backend" || r.str("root_id") != org.ID || !id.Valid(g1) {
		t.Fatalf("group %v", r.body)
	}
	if r.headers[auditx.CreatedHeader] != g1 {
		t.Fatalf("created header %q", r.headers[auditx.CreatedHeader])
	}

	r = e.call("POST", "/nodes/"+org.ID+"/groups", "", `{"name": "Ops"}`)
	e.expect(r, 201, "create second group")
	g2 := r.str("id")

	r = e.call("GET", "/nodes/"+org.ID+"/groups", "", "")
	e.expect(r, 200, "list groups")
	if got := ids(r.list("groups")); !slices.Equal(got, []string{g1, g2}) {
		t.Fatalf("groups %v", got)
	}

	r = e.call("PATCH", "/nodes/"+org.ID+"/groups/"+g1, "", `{"name": "Platform"}`)
	e.expect(r, 200, "rename group")
	if r.str("name") != "Platform" || !strings.Contains(r.headers[auditx.Header], `"name":{"from":"Backend","to":"Platform"}`) {
		t.Fatalf("rename %v %s", r.body, r.headers[auditx.Header])
	}
	e.expect(e.call("GET", "/nodes/"+other.ID+"/groups/"+g1, "", ""), 404, "group in another tree")

	// Members: people and service accounts in the same tree can join; outsiders cannot.
	bob := e.join(org, "bob", "nobody")
	sa, saKey := e.serviceAccount(p1, "viewer")
	e.expect(e.call("POST", "/nodes/"+org.ID+"/groups/"+g1+"/members", "", fmt.Sprintf(`{"member_id": %q}`, outsider)), 422, "outsider")
	e.expect(e.call("POST", "/nodes/"+org.ID+"/groups/"+g1+"/members", "", fmt.Sprintf(`{"member_id": %q}`, bob)), 201, "add bob to g1")
	e.expect(e.call("POST", "/nodes/"+org.ID+"/groups/"+g1+"/members", "", fmt.Sprintf(`{"member_id": %q}`, bob)), 409, "duplicate member")
	e.expect(e.call("POST", "/nodes/"+org.ID+"/groups/"+g1+"/members", "", fmt.Sprintf(`{"member_id": %q}`, sa)), 201, "add sa to g1")
	e.expect(e.call("POST", "/nodes/"+org.ID+"/groups/"+g2+"/members", "", fmt.Sprintf(`{"member_id": %q}`, bob)), 201, "add bob to g2")

	r = e.call("GET", "/nodes/"+org.ID+"/groups/"+g1+"/members", "", "")
	e.expect(r, 200, "list group members")
	if got := ids(r.list("members")); !slices.Equal(got, sorted(bob, sa)) {
		t.Fatalf("group members %v", got)
	}

	// Unified /bindings validation: neither or both is 422, group from another tree is 422.
	e.expect(e.call("POST", "/nodes/"+p1.ID+"/bindings", "", `{"role": "viewer"}`), 422, "neither subject")
	e.expect(e.call("POST", "/nodes/"+p1.ID+"/bindings", "", fmt.Sprintf(`{"member_id": %q, "group_id": %q, "role": "viewer"}`, bob, g1)), 422, "both subjects")
	otherGroup := e.call("POST", "/nodes/"+other.ID+"/groups", "", `{"name": "Other"}`).str("id")
	e.expect(e.call("POST", "/nodes/"+p1.ID+"/bindings", "", fmt.Sprintf(`{"group_id": %q, "role": "viewer"}`, otherGroup)), 422, "group from other tree")

	// Bind g1 as developer on folder f, and g2 as admin on project p1.
	r = e.call("POST", "/nodes/"+f.ID+"/bindings", "", fmt.Sprintf(`{"group_id": %q, "role": "developer"}`, g1))
	e.expect(r, 201, "bind g1 on folder")
	gb1 := r.str("id")
	if r.body["member_id"] != nil || r.str("group_id") != g1 || r.str("principal") != "group" || r.str("name") != "Platform" || r.body["guest"] != false {
		t.Fatalf("group binding %v", r.body)
	}
	e.expect(e.call("POST", "/nodes/"+f.ID+"/bindings", "", fmt.Sprintf(`{"group_id": %q, "role": "browser"}`, g1)), 409, "duplicate group binding")

	r = e.call("POST", "/nodes/"+p1.ID+"/bindings", "", fmt.Sprintf(`{"group_id": %q, "role": "admin"}`, g2))
	e.expect(r, 201, "bind g2 on p1")
	gb2 := r.str("id")

	// Access list at p1 includes inherited g1 binding and direct g2 binding.
	r = e.call("GET", "/nodes/"+p1.ID+"/bindings", "", "")
	e.expect(r, 200, "list bindings at p1")
	foundG1, foundG2 := false, false
	for _, b := range r.list("bindings") {
		if b["id"] == gb1 && b["group_id"] == g1 && b["member_id"] == nil && b["inherited"] == true && b["principal"] == "group" {
			foundG1 = true
		}
		if b["id"] == gb2 && b["group_id"] == g2 && b["member_id"] == nil && b["inherited"] == false && b["principal"] == "group" {
			foundG2 = true
		}
	}
	if !foundG1 || !foundG2 {
		t.Fatalf("missing group bindings in access list: %v", r.list("bindings"))
	}

	// Admit resolves group bindings for both people and service accounts, unioning multiple groups.
	bobWho := `"idp": {"issuer": "https://idp.test", "subject": "bob"}`
	a := e.admit(admitBody(bobWho, false, "organization", org.ID, "folder", f.ID, "project", p1.ID))
	if !slices.Contains(permissionsOf(a), "*") || !slices.Contains(permissionsOf(a), "project:write") {
		t.Fatalf("bob permissions on p1: %v", permissionsOf(a))
	}
	a = e.admit(admitBody(bobWho, false, "organization", org.ID, "folder", f.ID, "project", p2.ID))
	if !slices.Equal(permissionsOf(a), []string{"organization:read", "project:read", "project:write"}) {
		t.Fatalf("bob permissions on p2: %v", permissionsOf(a))
	}
	saWho := fmt.Sprintf(`"credential": %q`, saKey)
	a = e.admit(admitBody(saWho, false, "organization", org.ID, "folder", f.ID, "project", p2.ID))
	if !slices.Equal(permissionsOf(a), []string{"organization:read", "project:read", "project:write"}) {
		t.Fatalf("sa permissions on p2 via group: %v", permissionsOf(a))
	}

	// Membership at p1 includes bob's direct org binding plus both group bindings.
	r = e.call("GET", "/nodes/"+p1.ID+"/memberships/"+bob, "", "")
	e.expect(r, 200, "membership")
	if got := ids(r.list("bindings")); !slices.Contains(got, gb1) || !slices.Contains(got, gb2) {
		t.Fatalf("membership bindings %v", got)
	}
	if !slices.Contains(permissionsOf(r), "*") {
		t.Fatalf("membership permissions %v", permissionsOf(r))
	}

	// Filtered child lists and user nodes see nodes reachable via group bindings.
	r = e.call("GET", "/nodes/"+f.ID+"/children/project", bob, "")
	e.expect(r, 200, "children via group")
	if got := ids(r.list("nodes")); !slices.Equal(got, sorted(p1.ID, p2.ID)) {
		t.Fatalf("children %v", got)
	}
	r = e.call("GET", "/users/"+e.userOf("bob")+"/nodes?kind=project", "", "")
	e.expect(r, 200, "user nodes via group")
	if got := ids(r.list("nodes")); !slices.Equal(got, sorted(p1.ID, p2.ID)) {
		t.Fatalf("user nodes %v", got)
	}

	// Removing bob from g2 drops g2's admin on p1 while keeping g1's developer on f.
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/groups/"+g2+"/members/"+bob, "", ""), 204, "remove bob from g2")
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/groups/"+g2+"/members/"+bob, "", ""), 204, "remove again")
	a = e.admit(admitBody(bobWho, false, "organization", org.ID, "folder", f.ID, "project", p1.ID))
	if slices.Contains(permissionsOf(a), "*") || !slices.Contains(permissionsOf(a), "project:write") {
		t.Fatalf("bob permissions after leaving g2: %v", permissionsOf(a))
	}

	// Patch and delete a group binding: audit details name group_id, and members stay active.
	r = e.call("PATCH", "/nodes/"+f.ID+"/bindings/"+gb1, "", `{"role": "browser"}`)
	e.expect(r, 200, "patch group binding")
	if r.str("role") != "browser" {
		t.Fatalf("patched %v", r.body)
	}
	r = e.call("DELETE", "/nodes/"+f.ID+"/bindings/"+gb1, "", "")
	e.expect(r, 204, "delete group binding")
	if !strings.Contains(r.headers[auditx.Header], `"group_id":"`+g1+`"`) {
		t.Fatalf("delete group binding audit %s", r.headers[auditx.Header])
	}
}

func TestGroupsGuestAndAutoRemovalAndPurge(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("alice"), "Gruow")
	f := e.child(org, "folder", "Apps")
	p := e.child(f, "project", "Web")

	// Carol joins as a guest via project invite.
	carolUser := e.person("carol")
	if _, err := e.store.Redeem(e.ctx, carolUser, e.invite(p, "viewer").Token); err != nil {
		t.Fatal(err)
	}
	carol := e.memberOf(org, "carol")

	// Initially carol is a guest.
	r := e.call("GET", "/nodes/"+org.ID+"/members/"+carol, "", "")
	if r.body["guest"] != true {
		t.Fatalf("expected guest=true, got %v", r.body)
	}

	// Add carol to a group bound at the root: guest flips to false.
	g := e.call("POST", "/nodes/"+org.ID+"/groups", "", `{"name": "AllStaff"}`).str("id")
	e.expect(e.call("POST", "/nodes/"+org.ID+"/groups/"+g+"/members", "", fmt.Sprintf(`{"member_id": %q}`, carol)), 201, "add carol")
	rootBinding := e.call("POST", "/nodes/"+org.ID+"/bindings", "", fmt.Sprintf(`{"group_id": %q, "role": "member"}`, g)).str("id")
	r = e.call("GET", "/nodes/"+org.ID+"/members/"+carol, "", "")
	if r.body["guest"] != false {
		t.Fatalf("expected guest=false with root group binding, got %v", r.body)
	}

	// Deleting carol's direct project binding keeps carol active because she is in group g.
	e.expect(e.call("DELETE", "/nodes/"+p.ID+"/bindings/"+e.bindingOf(carol, p.ID), "", ""), 204, "delete direct binding")
	if e.removed(carol) {
		t.Fatal("carol should stay active while in a group")
	}

	// Deleting the group's root binding makes carol a guest again, still active because she's in g.
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/bindings/"+rootBinding, "", ""), 204, "delete root group binding")
	if e.removed(carol) {
		t.Fatal("deleting a group binding should not remove group members")
	}
	r = e.call("GET", "/nodes/"+org.ID+"/members/"+carol, "", "")
	if r.body["guest"] != true {
		t.Fatalf("expected guest=true after root group binding removed, got %v", r.body)
	}

	// Deleting group g leaves carol with no direct bindings and no groups: carol is removed.
	before := e.lastEvent()
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/groups/"+g, "", ""), 204, "delete group")
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/groups/"+g, "", ""), 204, "delete group again")
	if !e.removed(carol) {
		t.Fatal("carol left with no bindings and no groups should be removed")
	}
	if got := e.eventsAfter(before); !slices.Equal(got, []string{"member.removed " + carol + " " + org.ID + " person"}) {
		t.Fatalf("events %v", got)
	}

	// Removing a member from their last group also removes them when they have no direct bindings.
	dana := e.join(org, "dana", "member")
	g2 := e.call("POST", "/nodes/"+org.ID+"/groups", "", `{"name": "Team"}`).str("id")
	e.expect(e.call("POST", "/nodes/"+org.ID+"/groups/"+g2+"/members", "", fmt.Sprintf(`{"member_id": %q}`, dana)), 201, "add dana")
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/bindings/"+e.bindingOf(dana, org.ID), "", ""), 204, "drop dana direct binding")
	if e.removed(dana) {
		t.Fatal("dana is still in g2")
	}
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/groups/"+g2+"/members/"+dana, "", ""), 204, "remove dana from g2")
	if !e.removed(dana) {
		t.Fatal("dana left with no bindings and no groups should be removed")
	}

	// Purging a child node with a group binding deletes the group binding cleanly and keeps the group.
	e.expect(e.call("POST", "/nodes/"+p.ID+"/bindings", "", fmt.Sprintf(`{"group_id": %q, "role": "viewer"}`, g2)), 201, "bind g2 on p")
	if _, err := e.nodes.Delete(e.ctx, e.snap(), p.ID); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(31 * 24 * time.Hour)
	if n, err := e.nodes.PurgeDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("purge project: %d %v", n, err)
	}
	if e.count(`SELECT count(*) FROM bindings WHERE group_id = $1`, g2) != 0 {
		t.Fatal("group binding on purged project was not deleted")
	}
	if e.count(`SELECT count(*) FROM groups WHERE id = $1`, g2) != 1 {
		t.Fatal("group should survive child purge")
	}

	// Purging the root deletes its groups.
	if _, err := e.nodes.Delete(e.ctx, e.snap(), org.ID); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(31 * 24 * time.Hour)
	if _, err := e.nodes.PurgeDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM groups WHERE root_id = $1`, org.ID) != 0 {
		t.Fatal("root purge should delete groups")
	}
}
