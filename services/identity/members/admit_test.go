package members

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plat5dev/plat5/identity/nodes"
)

// POST /internal/admit (docs/reference/api/internal.md#admit): one table, one
// case per way a request ends, each checked against the gateway's status
// table (docs/reference/gateway.md#admit).

// gatewayStatus is what the gateway answers for an admit response on a route
// requiring routeLabels ([] is any member), per its status table.
func gatewayStatus(r response, routeLabels []string) int {
	if r.status != 200 {
		return 503
	}
	for _, f := range []string{"valid", "found", "principal", "nodes", "member_id", "labels", "good_until"} {
		if _, ok := r.body[f]; !ok {
			return 503
		}
	}
	if r.body["valid"] != true {
		return 401
	}
	if r.body["found"] != true || r.body["member_id"] == nil {
		return 403
	}
	if len(routeLabels) == 0 {
		return 200
	}
	for _, l := range labelsOf(r) {
		if l == "*" || slices.Contains(routeLabels, l) {
			return 200
		}
	}
	return 403
}

func labelsOf(r response) []string {
	out := []string{}
	items, _ := r.body["labels"].([]any)
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

func keyBody(key, scope string, chain ...string) string {
	return admitBody(fmt.Sprintf(`"credential": %q`, key), scope, false, chain...)
}

func idpBody(subject, scope string, chain ...string) string {
	return admitBody(fmt.Sprintf(`"idp": {"issuer": "https://idp.test", "subject": %q}`, subject), scope, false, chain...)
}

// admitBody builds a body; chain is kind, ref pairs.
func admitBody(who, scope string, resolveDeleted bool, chain ...string) string {
	refs := []string{}
	for i := 0; i+1 < len(chain); i += 2 {
		refs = append(refs, fmt.Sprintf(`{"kind": %q, "ref": %q}`, chain[i], chain[i+1]))
	}
	return fmt.Sprintf(`{%s, "scope": %q, "chain": [%s], "resolve_deleted": %v}`, who, scope, strings.Join(refs, ", "), resolveDeleted)
}

func (e *env) key(owner KeyOwner) string {
	e.t.Helper()
	k, err := e.store.CreateKey(e.ctx, owner, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return k.Secret
}

func (e *env) serviceAccount(n *nodes.Node, role string) (string, string) {
	e.t.Helper()
	sa, _, err := e.store.CreateServiceAccount(e.ctx, e.snap(), n.ID, "ci", role, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return sa.ID, e.key(KeyOwner{NodeID: n.ID, MemberID: sa.ID})
}

// joinUntil adds a person to a root with an expiring role.
func (e *env) joinUntil(root *nodes.Node, subject, role string, until time.Time) string {
	e.t.Helper()
	m, _, err := e.store.AddMember(e.ctx, e.snap(), root.ID, AddMemberInput{Issuer: "https://idp.test", Subject: subject, Role: &role, ExpiresAt: &until})
	if err != nil {
		e.t.Fatalf("join %s: %v", subject, err)
	}
	return m.ID
}

func (e *env) dirMember(subject string) string {
	e.t.Helper()
	var memberID string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT m.id FROM members m JOIN idp_links l ON l.principal_id = m.principal_id
		WHERE l.subject = $1 AND m.root_id = $2 AND m.removed_at IS NULL
	`, subject, e.signIn.ID).Scan(&memberID); err != nil {
		e.t.Fatalf("directory member of %s: %v", subject, err)
	}
	return memberID
}

func TestAdmit(t *testing.T) {
	e := setup(t)
	now := e.clock.t

	// Gruow: alice owns it. gatheroom and old are projects under it; eng is a
	// folder with the project deep under it, and gone a purged project.
	alice := e.person("alice")
	gruow, aliceM := e.org(alice, "Gruow")
	gatheroom := e.child(gruow, "project", "gatheroom")
	eng := e.child(gruow, "folder", "eng")
	e.child(eng, "project", "deep")
	old := e.child(gruow, "project", "old")
	gone := e.child(gruow, "project", "gone")
	other, _ := e.org(e.person("erin"), "Other")
	otherProject := e.child(other, "project", "elsewhere")

	// bob: support at the org until +3d, developer on gatheroom, viewer on old.
	supportUntil := now.Add(72 * time.Hour)
	bobM := e.joinUntil(gruow, "bob", "support", supportUntil)
	e.bind(gatheroom, bobM, "developer", nil)
	e.bind(old, bobM, "viewer", nil)
	// carol: a guest, viewer on gatheroom only, through an invite below the root.
	carol := e.person("carol")
	viewer := "viewer"
	expires := now.Add(24 * time.Hour)
	inv, err := e.store.CreateInvite(e.ctx, e.snap(), gatheroom.ID, InviteInput{Role: &viewer, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Redeem(e.ctx, carol, inv.Token); err != nil {
		t.Fatal(err)
	}
	// dave: in the directory only. frank: suspended, with a personal key.
	e.person("dave")
	frank := e.person("frank")
	frankKey := e.key(KeyOwner{MemberID: frank})
	if _, _, err := e.store.SetStatus(e.ctx, frank, StatusSuspended); err != nil {
		t.Fatal(err)
	}
	aliceKey := e.key(KeyOwner{MemberID: alice})
	revoked, err := e.store.CreateKey(e.ctx, KeyOwner{MemberID: alice}, "revoked")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.RevokeKey(e.ctx, KeyOwner{MemberID: alice}, revoked.ID); err != nil {
		t.Fatal(err)
	}
	// A service account born at gatheroom with developer there, and one born
	// at old, whose delete revokes its key; the key is un-revoked by hand so
	// the born-node rule is what refuses it.
	saM, saKey := e.serviceAccount(gatheroom, "developer")
	_, oldSAKey := e.serviceAccount(old, "developer")
	if _, err := e.nodes.Delete(e.ctx, e.snap(), old.ID); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE api_keys SET revoked_at = NULL WHERE principal_id IN (SELECT id FROM principals WHERE born_node_id = $1)`, old.ID)
	if _, err := e.nodes.Delete(e.ctx, e.snap(), eng.ID); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE nodes SET status = 'purged' WHERE id = $1`, gone.ID)

	unknownKey := "plat5-k-2-" + strings.Repeat("A", 43)
	read := []string{"project:read"}
	write := []string{"project:write"}

	type want struct {
		status    int // the gateway's answer
		labels    []string
		nodes     []string
		member    string // "" for null, unless anyMember
		anyMember bool
		goodUntil *time.Time
	}
	cases := []struct {
		name  string
		body  string
		route []string
		want  want
	}{
		// valid: false → 401
		{"unknown key", keyBody(unknownKey, "project", "organization", "gruow", "project", "gatheroom"), read, want{status: 401}},
		{"key of another shape", keyBody("plat5-mk-1-abc", "project", "organization", "gruow", "project", "gatheroom"), read, want{status: 401}},
		{"key of another brand", keyBody("acme-k-2-"+strings.Repeat("A", 43), "project", "organization", "gruow", "project", "gatheroom"), read, want{status: 401}},
		{"revoked key", keyBody(revoked.Secret, "project", "organization", "gruow", "project", "gatheroom"), read, want{status: 401}},
		{"suspended person's key", keyBody(frankKey, "organization", "organization", "gruow"), nil, want{status: 401}},
		{"suspended person's token", idpBody("frank", "directory"), nil, want{status: 401}},
		{"service account born at a deleted node", keyBody(oldSAKey, "organization", "organization", "gruow"), nil, want{status: 401}},
		{"service account on a sign-in route", keyBody(saKey, "directory"), nil, want{status: 401}},

		// found: false → 403
		{"missing slug", keyBody(aliceKey, "project", "organization", "gruow", "project", "nope"), read, want{status: 403}},
		{"missing root", keyBody(aliceKey, "organization", "organization", "nope"), read, want{status: 403}},
		{"ref under the wrong parent", keyBody(aliceKey, "project", "organization", "gruow", "project", "deep"), read, want{status: 403}},
		{"id under the wrong parent", keyBody(aliceKey, "project", "organization", "gruow", "project", otherProject.ID), read, want{status: 403}},
		{"ref of the wrong kind", keyBody(aliceKey, "folder", "organization", "gruow", "folder", "gatheroom"), read, want{status: 403}},
		{"a child as root", keyBody(aliceKey, "project", "project", gatheroom.ID), read, want{status: 403}},
		{"malformed id", keyBody(aliceKey, "project", "organization", "gruow", "project", "n_nope"), read, want{status: 403}},
		{"deleted node", keyBody(aliceKey, "project", "organization", "gruow", "project", "old"), read, want{status: 403}},
		{"deleted ancestor, resolve_deleted", admitBody(fmt.Sprintf(`"credential": %q`, aliceKey), "project", true, "organization", "gruow", "folder", "eng", "project", "deep"), read, want{status: 403}},
		{"purged node, resolve_deleted", admitBody(fmt.Sprintf(`"credential": %q`, aliceKey), "project", true, "organization", "gruow", "project", "gone"), read, want{status: 403}},

		// member_id: null → 403
		{"person not in the tree", idpBody("dave", "organization", "organization", "gruow"), nil, want{status: 403, nodes: []string{gruow.ID}, labels: []string{}}},
		{"service account outside its tree", keyBody(saKey, "organization", "organization", "other"), nil, want{status: 403, nodes: []string{other.ID}, labels: []string{}}},

		// no route label in labels → 403
		{"labels miss the route", idpBody("bob", "project", "organization", "gruow", "project", "gatheroom"), []string{"project:delete"},
			want{status: 403, nodes: []string{gruow.ID, gatheroom.ID}, member: bobM, labels: []string{"project:read", "project:write"}, goodUntil: &supportUntil}},
		{"guest at the root", idpBody("carol", "organization", "organization", "gruow"), []string{"organization:read"}, want{status: 403, nodes: []string{gruow.ID}, anyMember: true, labels: []string{}}},
		{"service account's binding doesn't reach up", keyBody(saKey, "organization", "organization", "gruow"), read, want{status: 403, member: saM, nodes: []string{gruow.ID}, labels: []string{}}},

		// forwarded
		{"union of the chain, earliest expiry", idpBody("bob", "project", "organization", "gruow", "project", "gatheroom"), write,
			want{status: 200, nodes: []string{gruow.ID, gatheroom.ID}, member: bobM, labels: []string{"project:read", "project:write"}, goodUntil: &supportUntil}},
		{"by id, mixed with slugs", idpBody("bob", "project", "organization", gruow.ID, "project", gatheroom.ID), write,
			want{status: 200, nodes: []string{gruow.ID, gatheroom.ID}, member: bobM, labels: []string{"project:read", "project:write"}, goodUntil: &supportUntil}},
		{"* matches any label", keyBody(aliceKey, "project", "organization", "gruow", "project", "gatheroom"), write,
			want{status: 200, nodes: []string{gruow.ID, gatheroom.ID}, member: aliceM, labels: []string{"*"}}},
		{"[] admits a member with no labels", keyBody(saKey, "organization", "organization", "gruow"), []string{}, want{status: 200, member: saM, nodes: []string{gruow.ID}, labels: []string{}}},
		{"guest below the root", idpBody("carol", "project", "organization", "gruow", "project", "gatheroom"), read, want{status: 200, anyMember: true, nodes: []string{gruow.ID, gatheroom.ID}, labels: []string{"project:read"}}},
		{"service account in its tree", keyBody(saKey, "project", "organization", "gruow", "project", "gatheroom"), write,
			want{status: 200, member: saM, nodes: []string{gruow.ID, gatheroom.ID}, labels: []string{"project:read", "project:write"}}},
		{"deleted node, resolve_deleted: its own bindings count", admitBody(`"idp": {"issuer": "https://idp.test", "subject": "bob"}`, "project", true, "organization", "gruow", "project", "old"), read,
			want{status: 200, nodes: []string{gruow.ID, old.ID}, member: bobM, labels: []string{"project:read"}, goodUntil: &supportUntil}},
		{"sign-in route", idpBody("alice", "directory"), nil, want{status: 200, member: alice, nodes: []string{}, labels: []string{}}},
		{"sign-in route with a personal key", keyBody(aliceKey, "directory"), nil, want{status: 200, member: alice, nodes: []string{}, labels: []string{}}},

		// non-2xx → 503
		{"credential and idp", `{"credential": "x", "idp": {"issuer": "i", "subject": "s"}, "scope": "directory", "chain": []}`, nil, want{status: 503}},
		{"neither", `{"scope": "directory", "chain": []}`, nil, want{status: 503}},
		{"idp without subject", `{"idp": {"issuer": "i"}, "scope": "directory", "chain": []}`, nil, want{status: 503}},
		{"public scope", keyBody(aliceKey, "public"), nil, want{status: 503}},
		{"chain on a sign-in route", keyBody(aliceKey, "directory", "organization", "gruow"), nil, want{status: 503}},
		{"empty chain on a kind route", keyBody(aliceKey, "project"), nil, want{status: 503}},
		{"chain not ending at the scope", keyBody(aliceKey, "project", "organization", "gruow"), nil, want{status: 503}},
		{"not JSON", `{`, nil, want{status: 503}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.admit(c.body)
			if got := gatewayStatus(r, c.route); got != c.want.status {
				t.Fatalf("gateway status %d, want %d: %d %v", got, c.want.status, r.status, r.body)
			}
			if r.status != 200 {
				if r.status != 422 && c.want.status == 503 && c.name != "not JSON" {
					t.Fatalf("status %d, want 422", r.status)
				}
				return
			}
			switch {
			case r.body["valid"] != true:
				// Every field is still there, empty (gatewayStatus checked presence).
				if r.body["found"] != false || r.body["principal"] != nil || r.body["member_id"] != nil || r.body["good_until"] != nil ||
					len(labelsOf(r)) != 0 || len(nodeIDsOf(r)) != 0 {
					t.Fatalf("invalid answer not empty: %v", r.body)
				}
				return
			case r.body["found"] != true:
				if len(nodeIDsOf(r)) != 0 || r.body["member_id"] != nil || len(labelsOf(r)) != 0 {
					t.Fatalf("not-found answer not empty: %v", r.body)
				}
				return
			}
			if got := nodeIDsOf(r); !slices.Equal(got, c.want.nodes) {
				t.Fatalf("nodes %v, want %v", got, c.want.nodes)
			}
			if got := labelsOf(r); !slices.Equal(got, c.want.labels) {
				t.Fatalf("labels %v, want %v", got, c.want.labels)
			}
			member, _ := r.body["member_id"].(string)
			if (c.want.anyMember && member == "") || (!c.want.anyMember && member != c.want.member) {
				t.Fatalf("member_id %q, want %q", member, c.want.member)
			}
			var wantUntil any
			if c.want.goodUntil != nil {
				wantUntil = rfc(*c.want.goodUntil)
			}
			if r.body["good_until"] != wantUntil {
				t.Fatalf("good_until %v, want %v", r.body["good_until"], wantUntil)
			}
		})
	}
}

// Resolution drops bindings whose role is gone from the document
// or no longer bindable at the node's kind.
func TestAdmitRoleChanges(t *testing.T) {
	e := setup(t)
	alice := e.person("alice")
	gruow, _ := e.org(alice, "Gruow")
	project := e.child(gruow, "project", "gatheroom")
	bobM := e.join(gruow, "bob", "gone_here")
	e.bind(project, bobM, "viewer", nil)
	body := idpBody("bob", "project", "organization", "gruow", "project", "gatheroom")

	if got := labelsOf(e.admit(body)); !slices.Equal(got, []string{"project:read"}) {
		t.Fatalf("labels %v", got)
	}
	// viewer no longer bindable at project, gone_here removed.
	cfg := strings.Replace(testConfig, `"viewer": { "labels": ["project:read"], "bindable_at": ["project"] }`,
		`"viewer": { "labels": ["project:read"], "bindable_at": ["organization"] }`, 1)
	cfg = strings.Replace(cfg, `"gone_here": { "labels": ["project:read"], "bindable_at": ["organization"] }`,
		`"other": { "labels": [], "bindable_at": ["organization"] }`, 1)
	if cfg == testConfig {
		t.Fatal("config unchanged")
	}
	e.src.Store(parseConfig(t, cfg))
	r := e.admit(body)
	if got := labelsOf(r); len(got) != 0 || r.body["member_id"] != bobM {
		t.Fatalf("labels %v member %v, want none for %s", got, r.body["member_id"], bobM)
	}
	if gatewayStatus(r, []string{"project:read"}) != 403 {
		t.Fatalf("gateway let a dropped role through")
	}
}

// Expiry is exact: a binding past expires_at counts for nothing, and
// good_until is the earliest expiry among those that count.
func TestAdmitExpiry(t *testing.T) {
	e := setup(t)
	alice := e.person("alice")
	gruow, _ := e.org(alice, "Gruow")
	project := e.child(gruow, "project", "gatheroom")
	inHour, inDay := e.clock.t.Add(time.Hour), e.clock.t.Add(24*time.Hour)
	bobM := e.joinUntil(gruow, "bob", "support", inDay)
	e.bind(project, bobM, "developer", &inHour)
	body := idpBody("bob", "project", "organization", "gruow", "project", "gatheroom")

	r := e.admit(body)
	if r.body["good_until"] != rfc(inHour) || !slices.Contains(labelsOf(r), "project:write") {
		t.Fatalf("before: %v", r.body)
	}
	e.clock.advance(time.Hour)
	r = e.admit(body)
	if r.body["good_until"] != rfc(inDay) || !slices.Equal(labelsOf(r), []string{"project:read"}) {
		t.Fatalf("at the first expiry: %v", r.body)
	}
	e.clock.advance(24 * time.Hour)
	r = e.admit(body)
	if r.body["good_until"] != nil || len(labelsOf(r)) != 0 || r.body["member_id"] != bobM {
		t.Fatalf("after both: %v", r.body)
	}
}

// A new IdP subject becomes a person with a directory member at their first
// admit, on any route; the name claim sets the name, and a missing or empty
// one leaves it.
func TestAdmitFirstSignIn(t *testing.T) {
	e := setup(t)
	alice := e.person("alice")
	e.org(alice, "Gruow")
	before := e.lastEvent()

	r := e.admit(admitBody(`"idp": {"issuer": "https://idp.test", "subject": "newbie", "name": "New Bie"}`, "organization", false, "organization", "gruow"))
	if gatewayStatus(r, nil) != 403 || r.body["valid"] != true {
		t.Fatalf("first admit on an org route: %v", r.body)
	}
	dir := e.dirMember("newbie")
	if got := e.eventsAfter(before); !slices.Equal(got, []string{fmt.Sprintf("member.created %s %s person", dir, e.signIn.ID)}) {
		t.Fatalf("events %v", got)
	}
	name := func() string {
		r := e.call("GET", "/members/"+dir, "", "")
		return r.str("name")
	}
	if name() != "New Bie" {
		t.Fatalf("name %q", name())
	}

	r = e.admit(admitBody(`"idp": {"issuer": "https://idp.test", "subject": "newbie", "name": "Renamed"}`, "directory", false))
	if r.body["member_id"] != dir || name() != "Renamed" {
		t.Fatalf("second admit: %v, name %q", r.body, name())
	}
	for _, claim := range []string{`, "name": ""`, ``} {
		e.admit(admitBody(`"idp": {"issuer": "https://idp.test", "subject": "newbie"`+claim+`}`, "directory", false))
		if name() != "Renamed" {
			t.Fatalf("claim %q changed the name to %q", claim, name())
		}
	}
	if e.lastEvent() != before+1 {
		t.Fatal("a later admit wrote events")
	}
	// Same subject at another issuer is another person.
	r = e.admit(admitBody(`"idp": {"issuer": "https://other.test", "subject": "newbie"}`, "directory", false))
	if r.body["member_id"] == dir {
		t.Fatal("issuer ignored")
	}
}

// Concurrent first admits of one subject make one person.
func TestAdmitFirstSignInRace(t *testing.T) {
	e := setup(t)
	body := idpBody("racer", "directory")
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
		seen[r.body["member_id"]] = true
	}
	if len(seen) != 1 || e.count(`SELECT count(*) FROM principals`) != 1 {
		raw, _ := json.Marshal(seen)
		t.Fatalf("members %s", raw)
	}
}
