package members

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plat5dev/plat5/identity/internal/auditx"
)

func TestCreateInvite(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	f := e.child(org, "folder", "One")
	p := e.child(f, "project", "One")
	other, _ := e.org(e.person("erin"), "Acme")
	otherFolder := e.child(other, "folder", "X")

	r := e.call("POST", "/nodes/"+org.ID+"/invites", "", `{"role": "member", "max_uses": 2,
		"grants": [{"node_id": "`+p.ID+`", "role": "viewer"}]}`)
	e.expect(r, 201, "create")
	token := r.str("token")
	if !strings.HasPrefix(token, "inv_") || r.str("token_prefix") != token[:8] || r.str("role") != "member" ||
		r.str("status") != "active" || r.body["max_uses"] != float64(2) || r.str("expires_at") != e.clock.t.Add(7*24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("invite %v", r.body)
	}
	if g := r.list("grants"); len(g) != 1 || g[0]["node_id"] != p.ID || g[0]["role"] != "viewer" || len(g[0]) != 2 {
		t.Fatalf("grants %v", g)
	}
	if strings.Contains(r.headers[auditx.Header], token) {
		t.Fatal("token in audit details")
	}
	// Shown once: lists carry the prefix only, and it's stored as a hash.
	r = e.call("GET", "/nodes/"+org.ID+"/invites", "", "")
	if inv := r.list("invites"); len(inv) != 1 || inv[0]["token"] != nil || inv[0]["token_prefix"] != token[:8] {
		t.Fatalf("list %v", inv)
	}
	if e.count(`SELECT count(*) FROM invites WHERE token_hash = encode(sha256($1::bytea), 'hex')`, token) != 1 {
		t.Fatal("token not stored as its hash")
	}

	for body, want := range map[string]any{
		`{"role": "member", "max_uses": "unlimited"}`: "unlimited",
		`{"role": "member", "max_uses": null}`:        float64(1),
		`{"role": "member"}`:                          float64(1),
	} {
		r := e.call("POST", "/nodes/"+org.ID+"/invites", "", body)
		if r.status != 201 || r.body["max_uses"] != want {
			t.Fatalf("%s: %d max_uses %v, want %v", body, r.status, r.body["max_uses"], want)
		}
	}
	r = e.call("POST", "/nodes/"+org.ID+"/invites", "", `{"role": "member", "expires_in": "8h"}`)
	if r.status != 201 || r.str("expires_at") != e.clock.t.Add(8*time.Hour).Format(time.RFC3339) {
		t.Fatalf("expires_in: %d %v", r.status, r.body)
	}
	for _, d := range []string{"60s", "30d"} {
		e.expect(e.call("POST", "/nodes/"+org.ID+"/invites", "", `{"role": "member", "expires_in": "`+d+`"}`), 201, "expires_in "+d)
	}

	for _, c := range []struct{ node, body, msg string }{
		{org.ID, `{}`, "Choose a role."},
		{org.ID, `{"role": "member", "expires_in": "59s"}`, "Expiry must be between 60 seconds and 30 days."},
		{org.ID, `{"role": "member", "expires_in": "31d"}`, "Expiry must be between 60 seconds and 30 days."},
		{org.ID, `{"role": "member", "expires_in": "soon"}`, "Expiry must be between 60 seconds and 30 days."},
		{org.ID, `{"role": "member", "expires_in": ""}`, "Expiry must be between 60 seconds and 30 days."},
		{org.ID, `{"role": "member", "max_uses": 0}`, "Max uses must be at least 1, or unlimited."},
		{org.ID, `{"role": "member", "max_uses": -2}`, "Max uses must be at least 1, or unlimited."},
		{org.ID, `{"role": "member", "max_uses": "lots"}`, "Max uses must be at least 1, or unlimited."},
		{p.ID, `{}`, "Choose a role."},
		{p.ID, `{"role": "viewer", "grants": [{"node_id": "` + p.ID + `", "role": "viewer"}]}`, "Only an invite to the organization can grant more."},
		{org.ID, `{"role": "member", "grants": [{"node_id": "` + otherFolder.ID + `", "role": "browser"}]}`, "That doesn't look right."},
		{org.ID, `{"role": "member", "grants": [{"node_id": "` + org.ID + `", "role": "admin"}]}`, "That doesn't look right."},
		{org.ID, `{"role": "member", "grants": [{"node_id": "` + p.ID + `", "role": "viewer"}, {"node_id": "` + p.ID + `", "role": "admin"}]}`, "That doesn't look right."},
		{org.ID, `{"role": "member", "grants": [{"node_id": "` + p.ID + `", "role": "member"}]}`, "That role can't be given on a project."},
		{org.ID, `{"role": "member", "grants": [{"node_id": "` + p.ID + `"}]}`, "Choose a role."},
	} {
		r := e.call("POST", "/nodes/"+c.node+"/invites", "", c.body)
		if r.status != 422 || r.message() != c.msg {
			t.Fatalf("%s: %d %q", c.body, r.status, r.message())
		}
	}
}

func (e *env) inviteToken(nodeID, body string) (id, token string) {
	e.t.Helper()
	r := e.call("POST", "/nodes/"+nodeID+"/invites", "", body)
	e.expect(r, 201, "invite")
	return r.str("id"), r.str("token")
}

func (e *env) redeem(userID, token string) response {
	e.t.Helper()
	return e.call("POST", "/users/"+userID+"/invites/redeem", "", `{"token": "`+token+`"}`)
}

func TestRedeemAtARoot(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	f := e.child(org, "folder", "One")
	p := e.child(f, "project", "One")
	q := e.child(f, "project", "Two")
	_, token := e.inviteToken(org.ID, `{"max_uses": 2, "role": "developer",
		"grants": [{"node_id": "`+p.ID+`", "role": "viewer"}, {"node_id": "`+q.ID+`", "role": "admin"}]}`)

	// A newcomer: a member with the role at the root, plus each grant on a live node.
	if _, err := e.nodes.Delete(e.ctx, e.snap(), q.ID); err != nil {
		t.Fatal(err)
	}
	frank := e.person("frank")
	before := e.lastEvent()
	r := e.redeem(frank, token)
	e.expect(r, 200, "redeem")
	member := r.str("member_id")
	node, _ := r.body["node"].(map[string]any)
	if node["id"] != org.ID || node["kind"] != "organization" || len(node["ancestors"].([]any)) != 0 {
		t.Fatalf("redeem %v", r.body)
	}
	if got := e.eventsAfter(before); !slices.Equal(got, []string{"member.created " + member + " " + org.ID + " person"}) {
		t.Fatalf("events %v", got)
	}
	if r.headers[auditx.NodesHeader] != org.ID || r.headers[auditx.MemberHeader] != member {
		t.Fatalf("named nodes %v", r.headers)
	}
	if e.count(`SELECT count(*) FROM bindings WHERE member_id = $1 AND node_id = $2 AND role = 'developer'`, member, org.ID) != 1 ||
		e.count(`SELECT count(*) FROM bindings WHERE member_id = $1 AND node_id = $2 AND role = 'viewer'`, member, p.ID) != 1 {
		t.Fatal("role at the root and the live grant")
	}
	if e.count(`SELECT count(*) FROM bindings WHERE node_id = $1`, q.ID) != 0 {
		t.Fatal("a grant on a deleted node should be skipped")
	}

	// Already bound at the root: nothing changes, and it isn't a use.
	before = e.lastEvent()
	r = e.redeem(frank, token)
	e.expect(r, 200, "redeem again")
	if r.str("member_id") != member || len(e.eventsAfter(before)) != 0 || e.count(`SELECT use_count FROM invites WHERE token_hash = encode(sha256($1::bytea), 'hex')`, token) != 1 {
		t.Fatal("a no-op redeem changed something")
	}

	// Two uses of two: redeemed.
	e.expect(e.redeem(e.person("gina"), token), 200, "second use")
	r = e.redeem(e.person("hal"), token)
	if r.status != 409 || r.message() != "This invite has already been used." || r.body["error"].(map[string]any)["details"].(map[string]any)["value"] != "redeemed" {
		t.Fatalf("used up: %d %v", r.status, r.body)
	}
	e.expect(e.redeem(frank, "inv_nope"), 404, "unknown token")
	e.expect(e.redeem(member, token), 404, "redeem as a member id")
}

func TestRedeemBelowARootMakesAGuest(t *testing.T) {
	e := setup(t)
	dana := e.person("dana")
	org, admin := e.org(dana, "Gruow")
	f := e.child(org, "folder", "One")
	p := e.child(f, "project", "One")
	_, token := e.inviteToken(p.ID, `{"role": "viewer", "max_uses": "unlimited"}`)

	outsider := e.person("outside")
	r := e.redeem(outsider, token)
	e.expect(r, 200, "redeem")
	guest := r.str("member_id")
	node, _ := r.body["node"].(map[string]any)
	if node["id"] != p.ID || len(node["ancestors"].([]any)) != 2 {
		t.Fatalf("redeem %v", r.body)
	}
	if r.headers[auditx.NodesHeader] != org.ID+","+f.ID+","+p.ID || r.headers[auditx.MemberHeader] != guest {
		t.Fatalf("named chain %v", r.headers)
	}
	if m := e.call("GET", "/nodes/"+org.ID+"/members/"+guest, "", ""); m.body["guest"] != true {
		t.Fatalf("not a guest: %v", m.body)
	}
	// Someone already in the tree just gets the binding.
	r = e.redeem(dana, token)
	if r.status != 200 || r.str("member_id") != admin {
		t.Fatalf("member redeem %d %v", r.status, r.body)
	}
	if e.count(`SELECT count(*) FROM bindings WHERE member_id = $1 AND node_id = $2`, admin, p.ID) != 1 {
		t.Fatal("member not bound")
	}
}

func TestInviteEndings(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	f := e.child(org, "folder", "One")
	frank := e.person("frank")

	// Revoke is idempotent; a revoked token can't be redeemed.
	inviteID, token := e.inviteToken(org.ID, `{"role": "member"}`)
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/invites/"+inviteID, "", ""), 204, "revoke")
	r := e.call("DELETE", "/nodes/"+org.ID+"/invites/"+inviteID, "", "")
	if r.status != 204 || r.headers[auditx.Header] != "" {
		t.Fatal("a second revoke changed something")
	}
	if r := e.redeem(frank, token); r.status != 409 || r.message() != "This invite is no longer valid." {
		t.Fatalf("revoked: %d %s", r.status, r.message())
	}
	e.expect(e.call("DELETE", "/nodes/"+f.ID+"/invites/"+inviteID, "", ""), 404, "revoke on another node")

	// Expired: read as expired at once; the sweeper writes it.
	inviteID, token = e.inviteToken(org.ID, `{"role": "member", "expires_in": "2m"}`)
	e.clock.advance(3 * time.Minute)
	if r := e.redeem(frank, token); r.status != 409 || r.message() != "This invite has expired." {
		t.Fatalf("expired: %d %s", r.status, r.message())
	}
	if r := e.call("GET", "/nodes/"+org.ID+"/invites", "", ""); r.list("invites")[1]["status"] != "expired" {
		t.Fatalf("listed %v", r.list("invites"))
	}
	if n, err := e.store.ExpireInvites(e.ctx); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if e.count(`SELECT count(*) FROM invites WHERE id = $1 AND status = 'expired'`, inviteID) != 1 {
		t.Fatal("sweeper didn't mark it expired")
	}

	// A delete revokes nothing: while deleted the invite is refused, and
	// restored it works again.
	_, rootToken := e.inviteToken(org.ID, `{"role": "member"}`)
	_, folderToken := e.inviteToken(f.ID, `{"role": "browser"}`)
	if _, err := e.nodes.Delete(e.ctx, e.snap(), org.ID); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{rootToken, folderToken} {
		r := e.redeem(frank, tok)
		if r.status != 409 || r.body["error"].(map[string]any)["details"].(map[string]any)["field"] != "node" {
			t.Fatalf("on a deleted node: %d %v", r.status, r.body)
		}
	}
	if r := e.redeem(frank, folderToken); r.message() != "This invite's folder was deleted." {
		t.Fatalf("message %q", r.message())
	}
	if _, _, err := e.nodes.Restore(e.ctx, e.snap(), org.ID); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{rootToken, folderToken} {
		e.expect(e.redeem(frank, tok), 200, "after restore")
	}
}
