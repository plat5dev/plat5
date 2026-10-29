package members

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// POST /internal/admit (docs/reference/api/internal.md#admit): one table, one
// case per way a request ends, each checked against the gateway's status
// table (docs/concepts/access.md#how-a-request-is-decided).

// admitFields is every field of every answer.
var admitFields = []string{"valid", "principal", "key_id", "nodes", "member_id", "user_id", "permissions"}

// gatewayStatus is what the gateway answers for an admit response on a route
// requiring need (nil on a user route, "any_member" for any member).
func gatewayStatus(r response, need any) int {
	if r.status != 200 {
		return 503
	}
	for _, f := range admitFields {
		if _, ok := r.body[f]; !ok {
			return 503
		}
	}
	if r.body["valid"] != true {
		return 401
	}
	if need == nil {
		return 200
	}
	if r.body["member_id"] == nil {
		return 403
	}
	if need == "any_member" {
		return 200
	}
	for _, p := range permissionsOf(r) {
		if p == "*" || slices.Contains(need.([]string), p) {
			return 200
		}
	}
	return 403
}

func permissionsOf(r response) []string {
	out := []string{}
	items, _ := r.body["permissions"].([]any)
	for _, it := range items {
		out = append(out, it.(string))
	}
	return out
}

func nodeIDsOf(r response) []string {
	out := []string{}
	for _, n := range r.list("nodes") {
		out = append(out, n["id"].(string))
	}
	return out
}

func (e *env) admit(body string) response {
	e.t.Helper()
	return e.callInternal("POST", "/internal/admit", body)
}

func keyBody(key string, chain ...string) string {
	return admitBody(fmt.Sprintf(`"credential": %q`, key), false, chain...)
}

func idpBody(subject string, chain ...string) string {
	return admitBody(fmt.Sprintf(`"idp": {"issuer": "https://idp.test", "subject": %q}`, subject), false, chain...)
}

// admitBody builds a body; chain is kind, ref pairs.
func admitBody(who string, allowDeleted bool, chain ...string) string {
	refs := []string{}
	for i := 0; i+1 < len(chain); i += 2 {
		refs = append(refs, fmt.Sprintf(`{"kind": %q, "ref": %q}`, chain[i], chain[i+1]))
	}
	return fmt.Sprintf(`{%s, "chain": [%s], "allow_deleted": %v}`, who, strings.Join(refs, ", "), allowDeleted)
}

func TestAdmit(t *testing.T) {
	e := setup(t)

	// Gruow: alice is its admin. apps is a folder with projects web and old;
	// eng is a deleted folder with deep under it, and gone a purged project.
	alice := e.person("alice")
	gruow, aliceM := e.org(alice, "Gruow")
	apps := e.child(gruow, "folder", "apps")
	web := e.child(apps, "project", "web")
	old := e.child(apps, "project", "old")
	gone := e.child(apps, "project", "gone")
	eng := e.child(gruow, "folder", "eng")
	e.child(eng, "project", "deep")
	other, _ := e.org(e.person("erin"), "Other")
	otherApps := e.child(other, "folder", "apps")
	otherWeb := e.child(otherApps, "project", "web")

	// bob: developer at the org, so member's organization:read too; viewer
	// on old.
	bobM := e.join(gruow, "bob", "developer")
	e.bind(old, bobM, "viewer")
	// carol: a guest, viewer on web only, through an invite below the root.
	carol := e.person("carol")
	if _, err := e.store.Redeem(e.ctx, carol, e.invite(web, "viewer").Token); err != nil {
		t.Fatal(err)
	}
	// dave: a person in no tree. frank: suspended, with a personal key.
	dave := e.person("dave")
	frank := e.person("frank")
	frankKey := e.key(KeyOwner{UserID: frank})
	if _, _, err := e.store.SetStatus(e.ctx, frank, StatusSuspended); err != nil {
		t.Fatal(err)
	}
	aliceKey := e.key(KeyOwner{UserID: alice})
	revoked, err := e.store.CreateKey(e.ctx, KeyOwner{UserID: alice}, "revoked")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.RevokeKey(e.ctx, KeyOwner{UserID: alice}, revoked.ID); err != nil {
		t.Fatal(err)
	}
	// A service account that belongs to web with developer there, and one that
	// belongs to old, which is then deleted: its key isn't revoked, and is refused.
	saM, saKey := e.serviceAccount(web, "developer")
	_, oldSAKey := e.serviceAccount(old, "developer")
	if _, err := e.nodes.Delete(e.ctx, e.snap(), old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.nodes.Delete(e.ctx, e.snap(), eng.ID); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE nodes SET status = 'purged' WHERE id = $1`, gone.ID)
	var aliceKeyID string
	if err := e.pool.QueryRow(e.ctx, `SELECT k.id FROM api_keys k JOIN principals p ON p.id = k.principal_id WHERE p.user_id = $1 AND k.revoked_at IS NULL`, alice).Scan(&aliceKeyID); err != nil {
		t.Fatal(err)
	}

	unknownKey := "plat5-k-2-" + strings.Repeat("A", 43)
	read := []string{"project:read"}
	write := []string{"project:write"}
	org := func(ref string) []string { return []string{"organization", ref} }
	project := func(refs ...string) []string {
		return []string{"organization", refs[0], "folder", refs[1], "project", refs[2]}
	}
	webChain := project("gruow", "apps", "web")

	type want struct {
		status      int // the gateway's answer
		permissions []string
		nodes       []string
		member      string // "" for null, unless anyMember
		anyMember   bool
		user        string
		keyID       string
	}
	cases := []struct {
		name string
		body string
		need any
		want want
	}{
		// valid: false → 401
		{"unknown key", keyBody(unknownKey, webChain...), read, want{status: 401}},
		{"a value identity can't read", keyBody("not a key", webChain...), read, want{status: 401}},
		{"key of another brand", keyBody("acme-k-2-"+strings.Repeat("A", 43), webChain...), read, want{status: 401}},
		{"revoked key", keyBody(revoked.Secret, webChain...), read, want{status: 401}},
		{"suspended person's key", keyBody(frankKey, org("gruow")...), "any_member", want{status: 401}},
		{"suspended person's token", idpBody("frank"), nil, want{status: 401}},
		{"service account that belongs to a deleted node", keyBody(oldSAKey, org("gruow")...), "any_member", want{status: 401}},
		{"service account on a user route", keyBody(saKey), nil, want{status: 401}},

		// member_id: null, nodes empty → 403
		{"missing slug", keyBody(aliceKey, project("gruow", "apps", "nope")...), read, want{status: 403}},
		{"missing root", keyBody(aliceKey, org("nope")...), read, want{status: 403}},
		{"ref under the wrong parent", keyBody(aliceKey, project("gruow", "eng", "web")...), read, want{status: 403}},
		{"id under the wrong parent", keyBody(aliceKey, project("gruow", "apps", otherWeb.ID)...), read, want{status: 403}},
		{"ref of the wrong kind", keyBody(aliceKey, "organization", "gruow", "project", "apps"), read, want{status: 403}},
		{"a child as root", keyBody(aliceKey, "folder", apps.ID), read, want{status: 403}},
		{"malformed id", keyBody(aliceKey, project("gruow", "apps", "0nope")...), read, want{status: 403}},
		{"uppercase id", keyBody(aliceKey, project("gruow", "apps", strings.ToUpper(web.ID))...), read, want{status: 403}},
		{"deleted node", keyBody(aliceKey, project("gruow", "apps", "old")...), read, want{status: 403}},
		{"deleted ancestor, allow_deleted", admitBody(fmt.Sprintf(`"credential": %q`, aliceKey), true, project("gruow", "eng", "deep")...), read, want{status: 403}},
		{"purged node, allow_deleted", admitBody(fmt.Sprintf(`"credential": %q`, aliceKey), true, project("gruow", "apps", "gone")...), read, want{status: 403}},
		{"person not in the tree", idpBody("dave", org("gruow")...), "any_member", want{status: 403}},
		{"service account outside its tree", keyBody(saKey, org("other")...), "any_member", want{status: 403}},
		{"a kind the config doesn't have", keyBody(aliceKey, "team", "gruow"), "any_member", want{status: 403}},

		// no permission in common → 403
		{"permissions miss the route", idpBody("bob", webChain...), []string{"project:delete"},
			want{status: 403, nodes: []string{gruow.ID, apps.ID, web.ID}, member: bobM, permissions: []string{"organization:read", "project:read", "project:write"}}},
		{"guest at the root", idpBody("carol", org("gruow")...), []string{"organization:read"}, want{status: 403, nodes: []string{gruow.ID}, anyMember: true, permissions: []string{}}},
		{"service account's binding doesn't reach up", keyBody(saKey, org("gruow")...), read, want{status: 403, member: saM, nodes: []string{gruow.ID}, permissions: []string{}}},

		// forwarded
		{"union of the chain, includes resolved", idpBody("bob", webChain...), write,
			want{status: 200, nodes: []string{gruow.ID, apps.ID, web.ID}, member: bobM, permissions: []string{"organization:read", "project:read", "project:write"}}},
		{"by id, mixed with slugs", idpBody("bob", project(gruow.ID, "apps", web.ID)...), write,
			want{status: 200, nodes: []string{gruow.ID, apps.ID, web.ID}, member: bobM, permissions: []string{"organization:read", "project:read", "project:write"}}},
		{"* matches any permission", keyBody(aliceKey, webChain...), write,
			want{status: 200, nodes: []string{gruow.ID, apps.ID, web.ID}, member: aliceM, permissions: []string{"*"}, keyID: aliceKeyID}},
		{"any_member admits a member with no permissions", keyBody(saKey, org("gruow")...), "any_member", want{status: 200, member: saM, nodes: []string{gruow.ID}, permissions: []string{}}},
		{"any_member admits a guest", idpBody("carol", org("gruow")...), "any_member", want{status: 200, anyMember: true, nodes: []string{gruow.ID}, permissions: []string{}}},
		{"guest below the root", idpBody("carol", webChain...), read, want{status: 200, anyMember: true, nodes: []string{gruow.ID, apps.ID, web.ID}, permissions: []string{"project:read"}}},
		{"service account in its tree", keyBody(saKey, webChain...), write,
			want{status: 200, member: saM, nodes: []string{gruow.ID, apps.ID, web.ID}, permissions: []string{"organization:read", "project:read", "project:write"}}},
		{"deleted node, allow_deleted: its own bindings count", admitBody(`"idp": {"issuer": "https://idp.test", "subject": "bob"}`, true, project("gruow", "apps", "old")...), []string{"project:read"},
			want{status: 200, nodes: []string{gruow.ID, apps.ID, old.ID}, member: bobM, permissions: []string{"organization:read", "project:read", "project:write"}}},
		{"user route", idpBody("alice"), nil, want{status: 200, user: alice}},
		{"user route with a personal key", keyBody(aliceKey), nil, want{status: 200, user: alice, keyID: aliceKeyID}},
		{"user route for a person in no tree", idpBody("dave"), nil, want{status: 200, user: dave}},

		// non-2xx → 503
		{"credential and idp", `{"credential": "x", "idp": {"issuer": "i", "subject": "s"}, "chain": []}`, nil, want{status: 503}},
		{"neither", `{"chain": []}`, nil, want{status: 503}},
		{"idp without subject", `{"idp": {"issuer": "i"}, "chain": []}`, nil, want{status: 503}},
		{"not JSON", `{`, nil, want{status: 503}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.admit(c.body)
			if got := gatewayStatus(r, c.need); got != c.want.status {
				t.Fatalf("gateway status %d, want %d: %d %v", got, c.want.status, r.status, r.body)
			}
			if r.status != 200 {
				if c.name != "not JSON" && r.status != 422 {
					t.Fatalf("status %d, want 422", r.status)
				}
				return
			}
			if r.body["valid"] != true {
				// Every field is still there, empty (gatewayStatus checked presence).
				if r.body["principal"] != nil || r.body["key_id"] != nil || r.body["member_id"] != nil || r.body["user_id"] != nil ||
					len(permissionsOf(r)) != 0 || len(nodeIDsOf(r)) != 0 {
					t.Fatalf("invalid answer not empty: %v", r.body)
				}
				return
			}
			if got := nodeIDsOf(r); !slices.Equal(got, c.want.nodes) && !(len(got) == 0 && c.want.nodes == nil) {
				t.Fatalf("nodes %v, want %v", got, c.want.nodes)
			}
			if got := permissionsOf(r); !slices.Equal(got, c.want.permissions) && !(len(got) == 0 && c.want.permissions == nil) {
				t.Fatalf("permissions %v, want %v", got, c.want.permissions)
			}
			member, _ := r.body["member_id"].(string)
			if (c.want.anyMember && member == "") || (!c.want.anyMember && member != c.want.member) {
				t.Fatalf("member_id %q, want %q", member, c.want.member)
			}
			if user, _ := r.body["user_id"].(string); user != c.want.user {
				t.Fatalf("user_id %q, want %q", user, c.want.user)
			}
			if c.want.keyID != "" && r.body["key_id"] != c.want.keyID {
				t.Fatalf("key_id %v, want %s", r.body["key_id"], c.want.keyID)
			}
			if strings.HasPrefix(c.body, `{"idp"`) && r.body["key_id"] != nil {
				t.Fatalf("a JWT has no key_id: %v", r.body)
			}
		})
	}
}

// Resolution drops bindings whose role is gone from the document or no
// longer bindable at the node's kind.
func TestAdmitRoleChanges(t *testing.T) {
	e := setup(t)
	gruow, _ := e.org(e.person("alice"), "Gruow")
	apps := e.child(gruow, "folder", "apps")
	web := e.child(apps, "project", "web")
	bobM := e.join(gruow, "bob", "gone_here")
	e.bind(web, bobM, "viewer")
	body := idpBody("bob", "organization", "gruow", "folder", "apps", "project", "web")

	if got := permissionsOf(e.admit(body)); !slices.Equal(got, []string{"project:read"}) {
		t.Fatalf("permissions %v", got)
	}
	// viewer no longer bindable at project, gone_here removed.
	cfg := strings.Replace(testConfig, `"viewer": { "permissions": ["project:read"], "bindable_at": ["project"] }`,
		`"viewer": { "permissions": ["project:read"], "bindable_at": ["organization"] }`, 1)
	cfg = strings.Replace(cfg, `"gone_here": { "permissions": ["project:read"], "bindable_at": ["organization"] }`,
		`"other": { "permissions": [], "bindable_at": ["organization"] }`, 1)
	if cfg == testConfig {
		t.Fatal("config unchanged")
	}
	e.src.Store(parseConfig(t, cfg))
	r := e.admit(body)
	if got := permissionsOf(r); len(got) != 0 || r.body["member_id"] != bobM {
		t.Fatalf("permissions %v member %v, want none for %s", got, r.body["member_id"], bobM)
	}
	if gatewayStatus(r, []string{"project:read"}) != 403 {
		t.Fatalf("gateway let a dropped role through")
	}
}

// A new IdP subject becomes a person with a user id at their first admit, on
// any route, with no member anywhere, and user.created; the name claim seeds
// the name, and a later sign-in leaves it (first_sign_in, the default).
func TestAdmitFirstSignIn(t *testing.T) {
	e := setup(t)
	e.org(e.person("alice"), "Gruow")
	before, beforeUsers := e.lastEvent(), e.lastUserEvent()

	r := e.admit(admitBody(`"idp": {"issuer": "https://idp.test", "subject": "newbie", "name": "New Bie"}`, false, "organization", "gruow"))
	if gatewayStatus(r, "any_member") != 403 || r.body["valid"] != true || r.body["user_id"] != nil {
		t.Fatalf("first admit on an org route: %v", r.body)
	}
	user := e.userOf("newbie")
	if got := e.eventsAfter(before); len(got) != 0 {
		t.Fatalf("a first sign-in makes no member: %v", got)
	}
	if got := e.userEventsAfter(beforeUsers); !slices.Equal(got, []string{"user.created " + user}) {
		t.Fatalf("user events %v", got)
	}
	name := func() string {
		return e.call("GET", "/users/"+user, "", "").str("name")
	}
	if name() != "New Bie" {
		t.Fatalf("name %q", name())
	}

	r = e.admit(admitBody(`"idp": {"issuer": "https://idp.test", "subject": "newbie", "name": "Renamed"}`, false))
	if r.body["user_id"] != user || name() != "New Bie" {
		t.Fatalf("second admit: %v, name %q", r.body, name())
	}
	for _, claim := range []string{`, "name": ""`, ``} {
		e.admit(admitBody(`"idp": {"issuer": "https://idp.test", "subject": "newbie"`+claim+`}`, false))
		if name() != "New Bie" {
			t.Fatalf("claim %q changed the name to %q", claim, name())
		}
	}
	if got := e.userEventsAfter(beforeUsers); len(got) != 1 {
		t.Fatalf("a later sign-in wrote user events: %v", got)
	}
	// Same subject at another issuer is another person.
	r = e.admit(admitBody(`"idp": {"issuer": "https://other.test", "subject": "newbie"}`, false))
	if r.body["user_id"] == user {
		t.Fatal("issuer ignored")
	}
	if got := e.userEventsAfter(beforeUsers); !slices.Equal(got, []string{"user.created " + user, "user.created " + r.body["user_id"].(string)}) {
		t.Fatalf("user events %v", got)
	}
}

// Concurrent first admits of one subject make one person.
func TestAdmitFirstSignInRace(t *testing.T) {
	e := setup(t)
	body := idpBody("racer")
	done := make(chan response, 8)
	for range 8 {
		go func() {
			done <- e.admit(body)
		}()
	}
	seen := map[any]bool{}
	for range 8 {
		r := <-done
		if r.status != 200 {
			t.Fatalf("status %d: %v", r.status, r.body)
		}
		seen[r.body["user_id"]] = true
	}
	if len(seen) != 1 || e.count(`SELECT count(*) FROM principals`) != 1 {
		raw, _ := json.Marshal(seen)
		t.Fatalf("users %s", raw)
	}
	if got := e.userEventsAfter(0); !slices.Equal(got, []string{"user.created " + e.userOf("racer")}) {
		t.Fatalf("user events %v", got)
	}
}
