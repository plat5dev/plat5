package members

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/id"
)

func TestAddMember(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	before, beforeUsers := e.lastEvent(), e.lastUserEvent()

	// A new IdP subject: the person is made too, with a user id, user.created
	// and no other member, in the add's transaction.
	r := e.call("POST", "/nodes/"+org.ID+"/members", "", `{"idp": {"issuer": "https://idp.test", "subject": "new"}, "role": "member"}`)
	e.expect(r, 201, "add")
	memberID := r.str("id")
	if r.str("principal") != "person" || r.str("status") != "active" || r.body["guest"] != false || r.body["name"] != nil || !id.Valid(memberID) {
		t.Fatalf("member %v", r.body)
	}
	if got := e.eventsAfter(before); !slices.Equal(got, []string{"member.created " + memberID + " " + org.ID + " person"}) {
		t.Fatalf("events %v", got)
	}
	if n := e.count(`SELECT count(*) FROM bindings WHERE member_id = $1 AND node_id = $2 AND role = 'member'`, memberID, org.ID); n != 1 {
		t.Fatal("role not bound at the root")
	}
	if r.headers[auditx.CreatedHeader] != memberID {
		t.Fatalf("created %q", r.headers[auditx.CreatedHeader])
	}
	if e.userOf("new") == memberID {
		t.Fatal("a user id is not a member id")
	}
	if got := e.userEventsAfter(beforeUsers); !slices.Equal(got, []string{"user.created " + e.userOf("new")}) {
		t.Fatalf("user events %v", got)
	}
	if at := e.occurredAt(before, beforeUsers); len(at) != 1 {
		t.Fatalf("user.created and member.created in one transaction: %v", at)
	}

	r = e.call("POST", "/nodes/"+org.ID+"/members", "", `{"idp": {"issuer": "https://idp.test", "subject": "new"}, "role": "member"}`)
	if r.status != 409 || r.message() != "This person is already a member." {
		t.Fatalf("already a member: %d %v", r.status, r.body)
	}

	// A known subject reuses the person, with no user event.
	people := e.count(`SELECT count(*) FROM principals`)
	other, _ := e.org(e.person("erin"), "Acme")
	beforeUsers = e.lastUserEvent()
	e.expect(e.call("POST", "/nodes/"+other.ID+"/members", "", `{"idp": {"issuer": "https://idp.test", "subject": "new"}, "role": "developer"}`), 201, "same person, other org")
	if n := e.count(`SELECT count(*) FROM principals`); n != people+1 { // erin only
		t.Fatalf("principals %d, want %d", n, people+1)
	}
	if got := e.userEventsAfter(beforeUsers); len(got) != 0 {
		t.Fatalf("user events for a known person: %v", got)
	}

	// A refused add makes no one: no person, no user event.
	beforeUsers = e.lastUserEvent()
	e.expect(e.call("POST", "/nodes/"+other.ID+"/members", "", `{"idp": {"issuer": "https://idp.test", "subject": "refused"}, "role": "viewer"}`), 422, "role not bindable")
	if got := e.userEventsAfter(beforeUsers); len(got) != 0 || e.count(`SELECT count(*) FROM idp_links WHERE subject = 'refused'`) != 0 {
		t.Fatalf("a refused add wrote %v", got)
	}

	for _, c := range []struct{ body, msg string }{
		{`{"idp": {"issuer": "", "subject": ""}, "role": "member"}`, "Choose someone to add."},
		{`{"role": "member"}`, "Choose someone to add."},
		{`{"idp": {"issuer": "i", "subject": "a"}}`, "Choose a role."},
		{`{"idp": {"issuer": "i", "subject": "a"}, "role": "nope"}`, "That role doesn't exist."},
		{`{"idp": {"issuer": "i", "subject": "a"}, "role": "viewer"}`, "That role can't be given on this organization."},
	} {
		r := e.call("POST", "/nodes/"+org.ID+"/members", "", c.body)
		if r.status != 422 || r.message() != c.msg {
			t.Fatalf("%s: %d %q", c.body, r.status, r.message())
		}
	}

	// Only roots have members.
	f := e.child(org, "folder", "One")
	e.expect(e.call("POST", "/nodes/"+f.ID+"/members", "", `{"idp": {"issuer": "i", "subject": "b"}, "role": "member"}`), 404, "members below a root")
	e.expect(e.call("GET", "/nodes/"+f.ID+"/members", "", ""), 404, "list below a root")
}

func TestMembersAndGuests(t *testing.T) {
	e := setup(t)
	org, admin := e.org(e.person("dana"), "Gruow")
	f := e.child(org, "folder", "One")
	p := e.child(f, "project", "One")
	plain := e.join(org, "plain", "member")

	// A guest: a person whose only binding is below the root.
	guest := e.join(org, "guest", "member")
	e.bind(p, guest, "viewer")
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/bindings/"+e.bindingOf(guest, org.ID), "", ""), 204, "drop root binding")
	// A service account that belongs below the root is never a guest.
	sa, _ := e.serviceAccount(p, "viewer")

	r := e.call("GET", "/nodes/"+org.ID+"/members", "", "")
	e.expect(r, 200, "list")
	guests := map[string]bool{}
	for _, m := range r.list("members") {
		guests[m["id"].(string)] = m["guest"].(bool)
	}
	if len(guests) != 4 || guests[admin] || guests[plain] || !guests[guest] || guests[sa] {
		t.Fatalf("members %v", guests)
	}

	// Removing is final: bindings go, member.removed, a lookup says removed.
	before := e.lastEvent()
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/members/"+plain, "", ""), 204, "remove")
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
	if r.str("status") != "removed" || r.body["guest"] != false {
		t.Fatalf("removed member %v", r.body)
	}
	// Re-adding makes a new member id.
	if again := e.join(org, "plain", "member"); again == plain {
		t.Fatal("re-adding reused a member id")
	}

	e.expect(e.call("GET", "/nodes/"+org.ID+"/members/"+id.New(), "", ""), 404, "missing member")
	_, otherAdmin := e.org(e.person("erin"), "Acme")
	e.expect(e.call("GET", "/nodes/"+org.ID+"/members/"+otherAdmin, "", ""), 404, "member of another tree")

	// A service account's member goes only with the service account.
	r = e.call("DELETE", "/nodes/"+org.ID+"/members/"+sa, "", "")
	if r.status != 422 || r.message() != "Delete the service account to remove it." {
		t.Fatalf("remove service account member: %d %s", r.status, r.message())
	}
}

func TestBindings(t *testing.T) {
	e := setup(t)
	org, admin := e.org(e.person("dana"), "Gruow")
	f := e.child(org, "folder", "One")
	p := e.child(f, "project", "One")
	m := e.join(org, "dev", "member")
	_, outsider := e.org(e.person("erin"), "Acme")

	// Direct bindings are for members of the tree only: guests come through invites.
	for _, who := range []string{outsider, id.New(), "nope"} {
		r := e.call("POST", "/nodes/"+p.ID+"/bindings", "", `{"member_id": "`+who+`", "role": "viewer"}`)
		if r.status != 422 || r.message() != "Add this person to the organization first, or invite them." {
			t.Fatalf("bind %s: %d %s", who, r.status, r.message())
		}
	}
	r := e.call("POST", "/nodes/"+p.ID+"/bindings", "", `{"member_id": "`+m+`", "role": "viewer"}`)
	e.expect(r, 201, "bind")
	bindingID := r.str("id")
	if r.str("member_id") != m || r.str("node_id") != p.ID || r.body["inherited"] != false || r.body["guest"] != false ||
		r.str("principal") != "person" || r.str("role") != "viewer" {
		t.Fatalf("binding %v", r.body)
	}
	if _, ok := r.body["expires_at"]; ok {
		t.Fatal("bindings don't expire")
	}
	r = e.call("POST", "/nodes/"+p.ID+"/bindings", "", `{"member_id": "`+m+`", "role": "developer"}`)
	if r.status != 409 || r.message() != "This member already has a role here." {
		t.Fatalf("second binding: %d %s", r.status, r.message())
	}
	e.expect(e.call("POST", "/nodes/"+p.ID+"/bindings", "", `{"member_id": "`+m+`", "role": "member"}`), 422, "role not bindable at project")
	e.expect(e.call("POST", "/nodes/"+p.ID+"/bindings", "", `{"member_id": "`+m+`"}`), 422, "role required")

	// PATCH changes the role.
	r = e.call("PATCH", "/nodes/"+p.ID+"/bindings/"+bindingID, "", `{"role": "developer"}`)
	e.expect(r, 200, "patch")
	if r.str("role") != "developer" || !strings.Contains(r.headers[auditx.Header], `"role":{"from":"viewer","to":"developer"}`) {
		t.Fatalf("patched %v %s", r.body, r.headers[auditx.Header])
	}
	e.expect(e.call("PATCH", "/nodes/"+p.ID+"/bindings/"+bindingID, "", `{}`), 422, "patch without a role")
	e.expect(e.call("PATCH", "/nodes/"+org.ID+"/bindings/"+bindingID, "", `{"role": "admin"}`), 404, "binding on another node")

	// The access list: every binding that counts here, own and inherited.
	r = e.call("GET", "/nodes/"+p.ID+"/bindings", "", "")
	e.expect(r, 200, "access list")
	type entry struct {
		member, node string
		inherited    bool
	}
	var got []entry
	for _, b := range r.list("bindings") {
		got = append(got, entry{b["member_id"].(string), b["node_id"].(string), b["inherited"].(bool)})
	}
	want := []entry{{admin, org.ID, true}, {m, org.ID, true}, {m, p.ID, false}}
	slices.SortFunc(got, func(a, b entry) int { return strings.Compare(a.member+a.node, b.member+b.node) })
	slices.SortFunc(want, func(a, b entry) int { return strings.Compare(a.member+a.node, b.member+b.node) })
	if !slices.Equal(got, want) {
		t.Fatalf("access list %v, want %v", got, want)
	}
	r = e.call("GET", "/nodes/"+p.ID+"/bindings?inherited=false", "", "")
	if got := ids(r.list("bindings")); !slices.Equal(got, []string{bindingID}) {
		t.Fatalf("own bindings %v", got)
	}
	e.expect(e.call("GET", "/nodes/"+p.ID+"/bindings?inherited=maybe", "", ""), 422, "bad inherited")

	// A person's member left with no bindings is removed.
	orgBinding := e.bindingOf(m, org.ID)
	before := e.lastEvent()
	e.expect(e.call("DELETE", "/nodes/"+p.ID+"/bindings/"+bindingID, "", ""), 204, "delete project binding")
	if e.removed(m) {
		t.Fatal("still bound at the org")
	}
	r = e.call("DELETE", "/nodes/"+org.ID+"/bindings/"+orgBinding, "", "")
	e.expect(r, 204, "delete last binding")
	if !e.removed(m) || !strings.Contains(r.headers[auditx.Header], `"role":{"from":"member","to":null}`) {
		t.Fatalf("a person left with no bindings should be removed; details %s", r.headers[auditx.Header])
	}
	if got := e.eventsAfter(before); !slices.Equal(got, []string{"member.removed " + m + " " + org.ID + " person"}) {
		t.Fatalf("events %v", got)
	}
	r = e.call("DELETE", "/nodes/"+org.ID+"/bindings/"+orgBinding, "", "")
	if r.status != 204 || r.headers[auditx.Header] != "" {
		t.Fatalf("delete again: %d %s", r.status, r.headers[auditx.Header])
	}
	e.expect(e.call("DELETE", "/nodes/"+id.New()+"/bindings/"+orgBinding, "", ""), 404, "binding on a missing node")

	// A service account's member stays with no bindings, so a team can re-bind it.
	sa, _ := e.serviceAccount(p, "viewer")
	e.expect(e.call("DELETE", "/nodes/"+p.ID+"/bindings/"+e.bindingOf(sa, p.ID), "", ""), 204, "unbind service account")
	if e.removed(sa) {
		t.Fatal("a service account's member goes only with it")
	}
	e.expect(e.call("POST", "/nodes/"+org.ID+"/bindings", "", `{"member_id": "`+sa+`", "role": "developer"}`), 201, "rebind it anywhere in its tree")
}

func TestMembership(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	f := e.child(org, "folder", "One")
	p := e.child(f, "project", "One")
	q := e.child(f, "project", "Two")
	dev := e.join(org, "dev", "developer")
	e.bind(f, dev, "browser")
	e.bind(q, dev, "viewer")
	stale := e.join(org, "stale", "member")
	e.exec(`INSERT INTO bindings (id, member_id, node_id, role) VALUES ($1, $2, $3, 'gone_here')`, id.New(), stale, p.ID)

	r := e.call("GET", "/nodes/"+p.ID+"/memberships/"+dev, "", "")
	e.expect(r, 200, "membership")
	if r.str("member_id") != dev {
		t.Fatalf("membership %v", r.body)
	}
	var nodes []string
	for _, b := range r.list("bindings") {
		nodes = append(nodes, b["node_id"].(string))
		if b["inherited"] != true {
			t.Fatalf("both sit above the project: %v", b)
		}
	}
	if !slices.Equal(sorted(nodes...), sorted(org.ID, f.ID)) {
		t.Fatalf("bindings on %v", nodes)
	}
	if got := permissionsOf(r); !slices.Equal(got, []string{"folder:read", "organization:read", "project:read", "project:write"}) {
		t.Fatalf("permissions %v", got)
	}
	// A binding whose role doesn't count is listed and grants nothing.
	r = e.call("GET", "/nodes/"+p.ID+"/memberships/"+stale, "", "")
	if len(r.list("bindings")) != 2 || !slices.Equal(permissionsOf(r), []string{"organization:read"}) {
		t.Fatalf("stale membership %v", r.body)
	}
	_, otherAdmin := e.org(e.person("erin"), "Acme")
	e.expect(e.call("GET", "/nodes/"+p.ID+"/memberships/"+otherAdmin, "", ""), 404, "a member of another tree")
	e.expect(e.call("GET", "/nodes/"+p.ID+"/memberships/nope", "", ""), 404, "malformed member")
}

var keyRe = regexp.MustCompile(`^plat5-k-2-[A-Za-z0-9_-]{43}$`)

func TestServiceAccountsAndKeys(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	f := e.child(org, "folder", "One")
	p := e.child(f, "project", "One")
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
		t.Fatal("not bound where it belongs")
	}
	for _, c := range []struct{ body, msg string }{
		{`{"name": "x"}`, "Choose a role."},
		{`{"role": "viewer"}`, "Name is required."},
		{`{"name": "` + strings.Repeat("x", 129) + `", "role": "viewer"}`, "Name is too long."},
		{`{"name": "x", "role": "member"}`, "That role can't be given on this project."},
	} {
		r := e.call("POST", "/nodes/"+p.ID+"/service-accounts", "", c.body)
		if r.status != 422 || r.message() != c.msg {
			t.Fatalf("%s: %d %s", c.body, r.status, r.message())
		}
	}

	r = e.call("GET", "/nodes/"+p.ID+"/service-accounts", "", "")
	if got := ids(r.list("service_accounts")); !slices.Equal(got, []string{sa}) {
		t.Fatalf("list %v", got)
	}
	e.expect(e.call("GET", "/nodes/"+org.ID+"/service-accounts/"+sa, "", ""), 404, "doesn't belong here")
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
	if !keyRe.MatchString(secret) || r.str("name") != "Unnamed key" || r.str("key_prefix") != secret[:len("plat5-k-2-")+4] || !id.Valid(keyID) {
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
	if e.count(`SELECT count(*) FROM principals WHERE kind = 'service_account' AND node_id = $1`, p.ID) != 0 ||
		e.count(`SELECT count(*) FROM api_keys a JOIN members m ON m.principal_id = a.principal_id WHERE m.id = $1`, sa) != 0 {
		t.Fatal("principal or keys left behind")
	}
	r = e.call("GET", "/nodes/"+org.ID+"/members/"+sa, "", "")
	if r.str("status") != "removed" || r.body["name"] != nil || r.str("principal") != "service_account" {
		t.Fatalf("lookup %v", r.body)
	}
	e.expect(e.call("GET", "/nodes/"+p.ID+"/service-accounts/"+sa, "", ""), 404, "deleted")
	r = e.call("DELETE", "/nodes/"+p.ID+"/service-accounts/"+sa, "", "")
	if r.status != 204 || r.headers[auditx.Header] != "" || len(e.eventsAfter(before)) != 1 {
		t.Fatalf("delete again: %d %s", r.status, r.headers[auditx.Header])
	}
	e.expect(e.call("DELETE", "/nodes/"+p.ID+"/service-accounts/"+id.New(), "", ""), 404, "never a service account here")
}

func TestUsers(t *testing.T) {
	e := setup(t)
	dana := e.person("dana")
	gruow, danaGruow := e.org(dana, "Gruow")
	acme, _ := e.org(e.person("erin"), "Acme")
	danaAcme := e.join(acme, "dana", "developer")
	sa, _ := e.serviceAccount(acme, "developer")

	r := e.call("GET", "/users/"+dana, "", "")
	if r.status != 200 || r.str("status") != "active" || r.str("id") != dana {
		t.Fatalf("user %v", r.body)
	}
	e.expect(e.call("GET", "/users/"+danaAcme, "", ""), 404, "a member id isn't a user")

	// The user behind a member.
	for _, m := range []string{danaGruow, danaAcme} {
		if r := e.call("GET", "/members/"+m+"/user", "", ""); r.status != 200 || r.str("id") != dana {
			t.Fatalf("user of %s: %d %v", m, r.status, r.body)
		}
	}
	e.expect(e.call("GET", "/members/"+sa+"/user", "", ""), 404, "a service account's member")

	// Exact lookup by IdP link.
	r = e.call("GET", "/users?issuer=https://idp.test&subject=dana", "", "")
	if got := ids(r.list("users")); r.status != 200 || !slices.Equal(got, []string{dana}) || r.body["has_more"] != false {
		t.Fatalf("lookup %d %v", r.status, r.body)
	}
	r = e.call("GET", "/users?issuer=https://idp.test&subject=nobody", "", "")
	if r.status != 200 || len(r.list("users")) != 0 {
		t.Fatalf("lookup of no one %v", r.body)
	}
	e.expect(e.call("GET", "/users?subject=dana", "", ""), 422, "issuer required")
	e.expect(e.call("GET", "/users?issuer=https://idp.test", "", ""), 422, "subject required")

	// Suspension is on the person.
	r = e.call("PATCH", "/users/"+dana+"/status", "", `{"status": "suspended"}`)
	if r.status != 200 || r.str("status") != "suspended" || r.headers[auditx.Header] != `{"status":{"from":"active","to":"suspended"}}` {
		t.Fatalf("suspend %d %v %s", r.status, r.body, r.headers[auditx.Header])
	}
	e.expect(e.call("PATCH", "/users/"+dana+"/status", "", `{"status": "gone"}`), 422, "bad status")
	e.expect(e.call("PATCH", "/users/"+id.New()+"/status", "", `{"status": "active"}`), 404, "no such user")
	e.call("PATCH", "/users/"+dana+"/status", "", `{"status": "active"}`)

	// Personal keys.
	r = e.call("POST", "/users/"+dana+"/keys", "", `{"name": "laptop"}`)
	e.expect(r, 201, "personal key")
	if !keyRe.MatchString(r.str("key")) {
		t.Fatal(r.str("key"))
	}
	k := r.str("id")
	e.expect(e.call("GET", "/users/"+dana+"/keys", "", ""), 200, "list keys")
	e.expect(e.call("DELETE", "/users/"+dana+"/keys/"+k, "", ""), 204, "revoke")
	e.expect(e.call("DELETE", "/users/"+dana+"/keys/"+k, "", ""), 204, "revoke again")
	e.expect(e.call("POST", "/users/"+danaAcme+"/keys", "", `{}`), 404, "keys on a member id")

	// Deleting the account removes every member, deletes keys and links, keeps
	// the rows, and writes user.deleted, all in one transaction.
	before, beforeUsers := e.lastEvent(), e.lastUserEvent()
	e.expect(e.call("DELETE", "/users/"+dana, "", ""), 204, "delete account")
	got := e.eventsAfter(before)
	want := sorted(
		"member.removed "+danaGruow+" "+gruow.ID+" person",
		"member.removed "+danaAcme+" "+acme.ID+" person",
	)
	if !slices.Equal(sorted(got...), want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if got := e.userEventsAfter(beforeUsers); !slices.Equal(got, []string{"user.deleted " + dana}) {
		t.Fatalf("user events %v", got)
	}
	if at := e.occurredAt(before, beforeUsers); len(at) != 1 {
		t.Fatalf("member.removed and user.deleted in one transaction: %v", at)
	}
	if e.count(`SELECT count(*) FROM idp_links WHERE subject = 'dana'`) != 0 ||
		e.count(`SELECT count(*) FROM members WHERE id = ANY($1) AND removed_at IS NOT NULL AND principal_id IS NULL`, []string{danaGruow, danaAcme}) != 2 {
		t.Fatal("account not deleted cleanly")
	}
	r = e.call("GET", "/nodes/"+gruow.ID+"/members/"+danaGruow, "", "")
	if r.status != 200 || r.str("status") != "removed" || r.body["name"] != nil {
		t.Fatalf("a deleted account's member %v", r.body)
	}
	e.expect(e.call("GET", "/users/"+dana, "", ""), 404, "deleted account")
	e.expect(e.call("GET", "/members/"+danaGruow+"/user", "", ""), 404, "user of a deleted account")
	e.expect(e.call("DELETE", "/users/"+dana, "", ""), 204, "delete again")
	if len(e.eventsAfter(before)) != 2 || len(e.userEventsAfter(beforeUsers)) != 1 {
		t.Fatal("a second delete changed something")
	}
	e.expect(e.call("GET", "/users/"+dana+"/keys", "", ""), 404, "keys of a deleted account")
}

// Concurrent deletes of one account write its events once, whether or not
// the person has members (and so tree locks) to serialize them.
func TestDeleteAccountRace(t *testing.T) {
	e := setup(t)
	member := e.person("member")
	org, m := e.org(member, "Gruow")
	loner := e.person("loner")
	for _, userID := range []string{member, loner} {
		before, beforeUsers := e.lastEvent(), e.lastUserEvent()
		done := make(chan response, 8)
		for range 8 {
			go func() { done <- e.call("DELETE", "/users/"+userID, "", "") }()
		}
		for range 8 {
			if r := <-done; r.status != 204 {
				t.Fatalf("delete %s: %d %v", userID, r.status, r.body)
			}
		}
		if got := e.userEventsAfter(beforeUsers); !slices.Equal(got, []string{"user.deleted " + userID}) {
			t.Fatalf("user events %v", got)
		}
		want := []string{}
		if userID == member {
			want = []string{"member.removed " + m + " " + org.ID + " person"}
		}
		if got := e.eventsAfter(before); !slices.Equal(got, want) {
			t.Fatalf("tree events %v, want %v", got, want)
		}
	}
}
