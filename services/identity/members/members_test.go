package members

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plat5dev/plat5/identity/internal/auditx"
)

func TestAddMember(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	before := e.lastEvent()

	// A new IdP subject: the person and their directory member are made too.
	r := e.call("POST", "/nodes/"+org.ID+"/members", "", `{"idp": {"issuer": "https://idp.test", "subject": "new"}}`)
	e.expect(r, 201, "add")
	memberID := r.str("id")
	if r.str("principal") != "person" || r.str("status") != "active" || r.body["guest"] != false || r.body["name"] != nil {
		t.Fatalf("member %v", r.body)
	}
	var dirMember string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT d.id FROM members d JOIN members m ON m.principal_id = d.principal_id
		WHERE m.id = $1 AND d.root_id = $2`, memberID, e.signIn.ID).Scan(&dirMember); err != nil {
		t.Fatalf("directory member: %v", err)
	}
	got := e.eventsAfter(before)
	want := []string{
		"member.created " + dirMember + " " + e.signIn.ID + " person",
		"member.created " + memberID + " " + org.ID + " person",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	// role omitted: default_role at the root.
	if n := e.count(`SELECT count(*) FROM bindings WHERE member_id = $1 AND node_id = $2 AND role = 'member'`, memberID, org.ID); n != 1 {
		t.Fatal("default_role not bound")
	}
	if r.headers[auditx.CreatedHeader] != memberID {
		t.Fatalf("created %q", r.headers[auditx.CreatedHeader])
	}

	r = e.call("POST", "/nodes/"+org.ID+"/members", "", `{"idp": {"issuer": "https://idp.test", "subject": "new"}}`)
	e.expect(r, 409, "already a member")
	if r.message() != "This person is already a member." {
		t.Fatal(r.message())
	}

	// A known subject reuses the person.
	people := e.count(`SELECT count(*) FROM principals`)
	other, _ := e.org(e.person("erin"), "Acme")
	e.expect(e.call("POST", "/nodes/"+other.ID+"/members", "", `{"idp": {"issuer": "https://idp.test", "subject": "new"}, "role": "developer"}`), 201, "same person, other org")
	if n := e.count(`SELECT count(*) FROM principals`); n != people+1 { // erin only
		t.Fatalf("principals %d, want %d", n, people+1)
	}

	until := rfc(e.clock.t.Add(3 * 24 * time.Hour))
	for _, c := range []struct{ body, field, msg string }{
		{`{"idp": {"issuer": "", "subject": ""}}`, "idp", "Choose someone to add."},
		{`{}`, "idp", "Choose someone to add."},
		{`{"idp": {"issuer": "i", "subject": "a"}, "role": "nope"}`, "role", "That role doesn't exist."},
		{`{"idp": {"issuer": "i", "subject": "a"}, "role": "viewer"}`, "role", "That role can't be given on a organization."},
		{`{"idp": {"issuer": "i", "subject": "a"}, "role": "support"}`, "expires_at", "This role can be given for at most 7d."},
		{`{"idp": {"issuer": "i", "subject": "a"}, "role": "support", "expires_at": "` + rfc(e.clock.t.Add(8*24*time.Hour)) + `"}`, "expires_at", "This role can be given for at most 7d."},
		{`{"idp": {"issuer": "i", "subject": "a"}, "expires_at": "` + rfc(e.clock.t.Add(-time.Hour)) + `"}`, "expires_at", "That doesn't look right."},
	} {
		r := e.call("POST", "/nodes/"+org.ID+"/members", "", c.body)
		if r.status != 422 || r.message() != c.msg {
			t.Fatalf("%s: %d %q", c.body, r.status, r.message())
		}
	}
	r = e.call("POST", "/nodes/"+org.ID+"/members", "", `{"idp": {"issuer": "i", "subject": "a"}, "role": "support", "expires_at": "`+until+`"}`)
	e.expect(r, 201, "support within max_duration")

	// Only roots other than the sign-in node have members.
	p := e.child(org, "project", "One")
	e.expect(e.call("POST", "/nodes/"+p.ID+"/members", "", `{"idp": {"issuer": "i", "subject": "b"}}`), 404, "members below a root")
	e.expect(e.call("GET", "/nodes/"+p.ID+"/members", "", ""), 404, "list below a root")
	e.expect(e.call("GET", "/nodes/"+e.signIn.ID+"/members", "", ""), 404, "the directory")
}

func TestMembersAndGuests(t *testing.T) {
	e := setup(t)
	org, owner := e.org(e.person("dana"), "Gruow")
	p := e.child(org, "project", "One")
	plain := e.join(org, "plain", "member")

	// A guest: a member whose only binding is below the root.
	guest := e.join(org, "guest", "member")
	e.bind(p, guest, "viewer", nil)
	b := e.bindingOf(guest, org.ID)
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/bindings/"+b, "", ""), 204, "drop root binding")

	r := e.call("GET", "/nodes/"+org.ID+"/members", "", "")
	e.expect(r, 200, "list")
	guests := map[string]bool{}
	for _, m := range r.list("members") {
		guests[m["id"].(string)] = m["guest"].(bool)
	}
	if len(guests) != 3 || guests[owner] || guests[plain] || !guests[guest] {
		t.Fatalf("members %v", guests)
	}

	// Removing is final: bindings go, member.removed, a lookup says removed.
	before := e.lastEvent()
	r = e.call("DELETE", "/nodes/"+org.ID+"/members/"+plain, "", "")
	e.expect(r, 204, "remove")
	if !e.removed(plain) || e.count(`SELECT count(*) FROM bindings WHERE member_id = $1`, plain) != 0 {
		t.Fatal("removed member keeps bindings")
	}
	if got := e.eventsAfter(before); !slices.Equal(got, []string{"member.removed " + plain + " " + org.ID + " person"}) {
		t.Fatalf("events %v", got)
	}
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/members/"+plain, "", ""), 204, "remove again")
	if got := e.eventsAfter(before); len(got) != 1 {
		t.Fatalf("a second remove changed something: %v", got)
	}
	r = e.call("GET", "/nodes/"+org.ID+"/members/"+plain, "", "")
	e.expect(r, 200, "lookup removed")
	if r.str("status") != "removed" {
		t.Fatalf("status %v", r.body)
	}
	// Re-adding makes a new member id.
	again := e.join(org, "plain", "member")
	if again == plain {
		t.Fatal("re-adding reused a member id")
	}

	e.expect(e.call("GET", "/nodes/"+org.ID+"/members/m_01JAAAAAAAAAAAAAAAAAAAAAAA", "", ""), 404, "missing member")
	other, otherOwner := e.org(e.person("erin"), "Acme")
	_ = other
	e.expect(e.call("GET", "/nodes/"+org.ID+"/members/"+otherOwner, "", ""), 404, "member of another tree")

	// A service account's member goes only with the service account.
	sa := e.call("POST", "/nodes/"+p.ID+"/service-accounts", "", `{"name": "ci", "role": "viewer"}`).str("id")
	r = e.call("DELETE", "/nodes/"+org.ID+"/members/"+sa, "", "")
	if r.status != 422 || r.message() != "Delete the service account to remove it." {
		t.Fatalf("remove service account member: %d %s", r.status, r.message())
	}
}

func (e *env) bindingOf(memberID, nodeID string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM bindings WHERE member_id = $1 AND node_id = $2`, memberID, nodeID).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestBindings(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	p := e.child(org, "project", "One")
	m := e.join(org, "dev", "member")
	_, outsider := e.org(e.person("erin"), "Acme")

	// Direct bindings are for members of the tree only: guests come through invites.
	for _, who := range []string{outsider, "m_01JAAAAAAAAAAAAAAAAAAAAAAA", "nope"} {
		r := e.call("POST", "/nodes/"+p.ID+"/bindings", "", `{"member": "`+who+`", "role": "viewer"}`)
		if r.status != 422 || r.message() != "Add this person to the organization first, or invite them." {
			t.Fatalf("bind %s: %d %s", who, r.status, r.message())
		}
	}
	r := e.call("POST", "/nodes/"+p.ID+"/bindings", "", `{"member": "`+m+`", "role": "viewer"}`)
	e.expect(r, 201, "bind")
	bindingID := r.str("id")
	if r.str("member") != m || r.str("node_id") != p.ID || r.body["expires_at"] != nil {
		t.Fatalf("binding %v", r.body)
	}
	r = e.call("POST", "/nodes/"+p.ID+"/bindings", "", `{"member": "`+m+`", "role": "developer"}`)
	if r.status != 409 || r.message() != "This member already has a role here." {
		t.Fatalf("second binding: %d %s", r.status, r.message())
	}
	e.expect(e.call("POST", "/nodes/"+p.ID+"/bindings", "", `{"member": "`+m+`", "role": "member"}`), 422, "role not bindable at project")

	// PATCH changes role or expiry; the result must meet the role's rules.
	until := e.clock.t.Add(time.Hour)
	r = e.call("PATCH", "/nodes/"+p.ID+"/bindings/"+bindingID, "", `{"role": "developer", "expires_at": "`+rfc(until)+`"}`)
	e.expect(r, 200, "patch")
	if r.str("role") != "developer" || r.str("expires_at") != rfc(until) {
		t.Fatalf("patched %v", r.body)
	}
	if !strings.Contains(r.headers[auditx.Header], `"role":{"from":"viewer","to":"developer"}`) {
		t.Fatalf("details %s", r.headers[auditx.Header])
	}
	r = e.call("PATCH", "/nodes/"+p.ID+"/bindings/"+bindingID, "", `{"expires_at": null}`)
	e.expect(r, 200, "clear expiry")
	if r.body["expires_at"] != nil {
		t.Fatal("expires_at: null should clear it")
	}
	orgBinding := e.bindingOf(m, org.ID)
	r = e.call("PATCH", "/nodes/"+org.ID+"/bindings/"+orgBinding, "", `{"role": "support"}`)
	if r.status != 422 || r.message() != "This role can be given for at most 7d." {
		t.Fatalf("support without expiry: %d %s", r.status, r.message())
	}
	e.expect(e.call("PATCH", "/nodes/"+org.ID+"/bindings/"+bindingID, "", `{"role": "viewer"}`), 404, "binding on another node")

	// A person's member left with no bindings is removed.
	before := e.lastEvent()
	e.expect(e.call("DELETE", "/nodes/"+p.ID+"/bindings/"+bindingID, "", ""), 204, "delete project binding")
	if e.removed(m) {
		t.Fatal("still bound at the org")
	}
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/bindings/"+orgBinding, "", ""), 204, "delete last binding")
	if !e.removed(m) {
		t.Fatal("a person left with no bindings should be removed")
	}
	if got := e.eventsAfter(before); !slices.Equal(got, []string{"member.removed " + m + " " + org.ID + " person"}) {
		t.Fatalf("events %v", got)
	}
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/bindings/"+orgBinding, "", ""), 404, "deleted binding")

	// A service account's member stays with no bindings, so a team can re-bind it.
	sa := e.call("POST", "/nodes/"+p.ID+"/service-accounts", "", `{"name": "ci", "role": "viewer"}`).str("id")
	e.expect(e.call("DELETE", "/nodes/"+p.ID+"/bindings/"+e.bindingOf(sa, p.ID), "", ""), 204, "unbind service account")
	if e.removed(sa) {
		t.Fatal("a service account's member goes only with it")
	}
	e.expect(e.call("POST", "/nodes/"+org.ID+"/bindings", "", `{"member": "`+sa+`", "role": "developer"}`), 201, "rebind it anywhere in its tree")

	r = e.call("GET", "/nodes/"+org.ID+"/bindings", "", "")
	e.expect(r, 200, "list")
	if len(r.list("bindings")) != 2 { // the owner, the service account
		t.Fatalf("bindings %v", r.list("bindings"))
	}
}

func TestAccess(t *testing.T) {
	e := setup(t)
	org, owner := e.org(e.person("dana"), "Gruow")
	p := e.child(org, "project", "One")
	q := e.child(org, "project", "Two")
	viewer := e.join(org, "viewer", "member")
	e.bind(p, viewer, "viewer", nil)
	guest := e.join(org, "guest", "member")
	e.bind(q, guest, "viewer", nil)
	e.call("DELETE", "/nodes/"+org.ID+"/bindings/"+e.bindingOf(guest, org.ID), "", "")
	short := e.join(org, "short", "member")
	e.bind(p, short, "viewer", ptr(e.clock.t.Add(time.Minute)))
	e.clock.advance(time.Hour) // expired, not yet swept

	type entry struct {
		member, node string
		inherited    bool
		guest        bool
	}
	read := func(n string) []entry {
		t.Helper()
		r := e.call("GET", "/nodes/"+n+"/access", "", "")
		e.expect(r, 200, "access")
		var out []entry
		for _, a := range r.list("access") {
			out = append(out, entry{a["member"].(string), a["node_id"].(string), a["inherited"].(bool), a["guest"].(bool)})
		}
		return out
	}
	got := read(p.ID)
	want := map[entry]bool{
		{owner, org.ID, true, false}:  true,
		{viewer, org.ID, true, false}: true,
		{short, org.ID, true, false}:  true,
		{viewer, p.ID, false, false}:  true,
	}
	if len(got) != len(want) {
		t.Fatalf("project access %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("unexpected %v in %v", g, got)
		}
	}
	// On the root, guests' bindings below it are listed, marked.
	got = read(org.ID)
	found := false
	for _, g := range got {
		if g.member == guest {
			found = g.node == q.ID && g.guest && !g.inherited
		}
	}
	if !found {
		t.Fatalf("guest not on the root's access list: %v", got)
	}
}

var keyRe = regexp.MustCompile(`^plat5-k-2-[A-Za-z0-9_-]{43}$`)

func TestServiceAccountsAndKeys(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	p := e.child(org, "project", "One")
	before := e.lastEvent()

	r := e.call("POST", "/nodes/"+p.ID+"/service-accounts", "", `{"name": " CI ", "role": "developer"}`)
	e.expect(r, 201, "create")
	sa := r.str("id")
	if r.str("name") != "CI" || r.str("node_id") != p.ID {
		t.Fatalf("service account %v", r.body)
	}
	if got := e.eventsAfter(before); !slices.Equal(got, []string{"member.created " + sa + " " + org.ID + " service_account"}) {
		t.Fatalf("events %v", got)
	}
	if e.bindingOf(sa, p.ID) == "" {
		t.Fatal("not bound where born")
	}
	for _, c := range []struct{ body, msg string }{
		{`{"name": "x"}`, "Choose a role."},
		{`{"role": "viewer"}`, "Name is required."},
		{`{"name": "` + strings.Repeat("x", 129) + `", "role": "viewer"}`, "Name is too long."},
		{`{"name": "x", "role": "member"}`, "That role can't be given on a project."},
	} {
		r := e.call("POST", "/nodes/"+p.ID+"/service-accounts", "", c.body)
		if r.status != 422 || r.message() != c.msg {
			t.Fatalf("%s: %d %s", c.body, r.status, r.message())
		}
	}
	e.expect(e.call("POST", "/nodes/"+e.signIn.ID+"/service-accounts", "", `{"name": "x", "role": "owner"}`), 422, "at the sign-in node")
	e.expect(e.call("POST", "/nodes/"+org.ID+"/service-accounts", "", `{"name": "x", "role": "support"}`), 422, "support needs expires_at")
	e.expect(e.call("POST", "/nodes/"+org.ID+"/service-accounts", "", `{"name": "x", "role": "support", "expires_at": "`+rfc(e.clock.t.Add(time.Hour))+`"}`), 201, "support with expires_at")

	r = e.call("GET", "/nodes/"+p.ID+"/service-accounts", "", "")
	if got := ids(r.list("service_accounts")); !slices.Equal(got, []string{sa}) {
		t.Fatalf("list %v", got)
	}
	e.expect(e.call("GET", "/nodes/"+org.ID+"/service-accounts/"+sa, "", ""), 404, "not born here")
	r = e.call("PATCH", "/nodes/"+p.ID+"/service-accounts/"+sa, "", `{"name": "deploy"}`)
	if r.status != 200 || r.str("name") != "deploy" {
		t.Fatalf("rename %d %v", r.status, r.body)
	}
	if r := e.call("PATCH", "/nodes/"+p.ID+"/service-accounts/"+sa, "", `{}`); r.message() != "Nothing to update." {
		t.Fatal(r.message())
	}

	// Keys: shown once, listed by prefix, revoked idempotently.
	r = e.call("POST", "/nodes/"+p.ID+"/service-accounts/"+sa+"/keys", "", ``)
	e.expect(r, 201, "key")
	secret, keyID := r.str("key"), r.str("id")
	if !keyRe.MatchString(secret) || r.str("name") != "Unnamed key" || r.str("key_prefix") != secret[:len("plat5-k-2-")+4] {
		t.Fatalf("key %v", r.body)
	}
	if !strings.Contains(r.headers[auditx.Header], keyID) || strings.Contains(r.headers[auditx.Header], secret) {
		t.Fatalf("details %s", r.headers[auditx.Header])
	}
	r = e.call("GET", "/nodes/"+p.ID+"/service-accounts/"+sa+"/keys", "", "")
	if keys := r.list("keys"); len(keys) != 1 || keys[0]["key"] != nil {
		t.Fatalf("list keys %v", keys)
	}
	if n := e.count(`SELECT count(*) FROM api_keys WHERE key_hash = encode(sha256($1::bytea), 'hex')`, secret); n != 1 {
		t.Fatal("key not stored as its hash")
	}
	e.expect(e.call("DELETE", "/nodes/"+p.ID+"/service-accounts/"+sa+"/keys/"+keyID, "", ""), 204, "revoke")
	r = e.call("DELETE", "/nodes/"+p.ID+"/service-accounts/"+sa+"/keys/"+keyID, "", "")
	e.expect(r, 204, "revoke again")
	if r.headers[auditx.Header] != "" {
		t.Fatal("a second revoke changed something")
	}
	r = e.call("GET", "/nodes/"+p.ID+"/service-accounts/"+sa+"/keys", "", "")
	if keys := r.list("keys"); len(keys) != 1 || keys[0]["revoked_at"] == nil {
		t.Fatalf("revoked keys stay listed: %v", keys)
	}
	e.expect(e.call("POST", "/nodes/"+p.ID+"/service-accounts/"+sa+"/keys", "", `{"name": "`+strings.Repeat("k", 129)+`"}`), 422, "long key name")

	// Delete: as at purge, principal and keys go, member removed.
	e.call("POST", "/nodes/"+p.ID+"/service-accounts/"+sa+"/keys", "", `{"name": "live"}`)
	before = e.lastEvent()
	e.expect(e.call("DELETE", "/nodes/"+p.ID+"/service-accounts/"+sa, "", ""), 204, "delete")
	if got := e.eventsAfter(before); !slices.Equal(got, []string{"member.removed " + sa + " " + org.ID + " service_account"}) {
		t.Fatalf("events %v", got)
	}
	if e.count(`SELECT count(*) FROM principals WHERE kind = 'service_account' AND born_node_id = $1`, p.ID) != 0 ||
		e.count(`SELECT count(*) FROM api_keys a JOIN members m ON m.principal_id = a.principal_id WHERE m.id = $1`, sa) != 0 {
		t.Fatal("principal or keys left behind")
	}
	r = e.call("GET", "/nodes/"+org.ID+"/members/"+sa, "", "")
	if r.str("status") != "removed" || r.body["name"] != nil || r.str("principal") != "service_account" {
		t.Fatalf("lookup %v", r.body)
	}
	e.expect(e.call("GET", "/nodes/"+p.ID+"/service-accounts/"+sa, "", ""), 404, "deleted")
	e.expect(e.call("DELETE", "/nodes/"+p.ID+"/service-accounts/"+sa, "", ""), 404, "delete again")
}

func TestPeople(t *testing.T) {
	e := setup(t)
	dana := e.person("dana")
	gruow, _ := e.org(dana, "Gruow")
	acme, _ := e.org(e.person("erin"), "Acme")
	danaAcme := e.join(acme, "dana", "developer")

	r := e.call("GET", "/members/"+dana, "", "")
	if r.status != 200 || r.str("status") != "active" {
		t.Fatalf("person %v", r.body)
	}
	e.expect(e.call("GET", "/members/"+danaAcme, "", ""), 404, "not a directory member")

	// Suspension is on the person.
	r = e.call("PATCH", "/members/"+dana+"/status", "", `{"status": "suspended"}`)
	if r.status != 200 || r.str("status") != "suspended" {
		t.Fatalf("suspend %d %v", r.status, r.body)
	}
	e.expect(e.call("PATCH", "/members/"+dana+"/status", "", `{"status": "gone"}`), 422, "bad status")
	e.call("PATCH", "/members/"+dana+"/status", "", `{"status": "active"}`)

	// Personal keys.
	r = e.call("POST", "/members/"+dana+"/keys", "", `{"name": "laptop"}`)
	e.expect(r, 201, "personal key")
	if !keyRe.MatchString(r.str("key")) {
		t.Fatal(r.str("key"))
	}
	e.expect(e.call("POST", "/members/"+danaAcme+"/keys", "", `{}`), 404, "keys on an org member")

	// Deleting the account removes every member, deletes keys and links, keeps the rows.
	var danaGruow string
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM members WHERE root_id = $1`, gruow.ID).Scan(&danaGruow); err != nil {
		t.Fatal(err)
	}
	before := e.lastEvent()
	e.expect(e.call("DELETE", "/members/"+dana, "", ""), 204, "delete account")
	got := e.eventsAfter(before)
	want := sorted(
		"member.removed "+dana+" "+e.signIn.ID+" person",
		"member.removed "+danaGruow+" "+gruow.ID+" person",
		"member.removed "+danaAcme+" "+acme.ID+" person",
	)
	if !slices.Equal(sorted(got...), want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if e.count(`SELECT count(*) FROM idp_links WHERE subject = 'dana'`) != 0 ||
		e.count(`SELECT count(*) FROM members WHERE id = ANY($1) AND removed_at IS NOT NULL AND principal_id IS NULL`, []string{dana, danaGruow, danaAcme}) != 3 {
		t.Fatal("account not deleted cleanly")
	}
	r = e.call("GET", "/members/"+dana, "", "")
	if r.status != 200 || r.str("status") != "removed" || r.body["name"] != nil {
		t.Fatalf("deleted person %v", r.body)
	}
	e.expect(e.call("DELETE", "/members/"+dana, "", ""), 204, "delete again")
	if len(e.eventsAfter(before)) != 3 {
		t.Fatal("a second delete changed something")
	}
	e.expect(e.call("GET", "/members/"+dana+"/keys", "", ""), 404, "keys of a deleted account")
}
