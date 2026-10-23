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
	p := e.child(org, "project", "One")
	other, _ := e.org(e.person("erin"), "Acme")
	otherProject := e.child(other, "project", "X")
	day := rfc(e.clock.t.Add(24 * time.Hour))

	r := e.call("POST", "/nodes/"+org.ID+"/invites", "", `{"expires_at": "`+day+`", "max_uses": 2,
		"grants": [{"node": "`+p.ID+`", "role": "viewer"}]}`)
	e.expect(r, 201, "create")
	token := r.str("token")
	if !strings.HasPrefix(token, "inv_") || r.str("token_prefix") != token[:8] || r.str("role") != "member" ||
		r.str("status") != "active" || r.body["max_uses"] != float64(2) {
		t.Fatalf("invite %v", r.body)
	}
	if g := r.list("grants"); len(g) != 1 || g[0]["node"] != p.ID || g[0]["role"] != "viewer" {
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

	r = e.call("POST", "/nodes/"+org.ID+"/invites", "", `{"expires_at": "`+day+`", "max_uses": null}`)
	if r.status != 201 || r.body["max_uses"] != nil {
		t.Fatalf("unlimited %d %v", r.status, r.body)
	}
	r = e.call("POST", "/nodes/"+org.ID+"/invites", "", `{"expires_at": "`+day+`"}`)
	if r.body["max_uses"] != float64(1) {
		t.Fatalf("max_uses omitted → 1: %v", r.body["max_uses"])
	}

	for _, c := range []struct{ node, body, msg string }{
		{org.ID, `{}`, "Expiry must be between 60 seconds and 30 days."},
		{org.ID, `{"expires_at": "` + rfc(e.clock.t.Add(30*time.Second)) + `"}`, "Expiry must be between 60 seconds and 30 days."},
		{org.ID, `{"expires_at": "` + rfc(e.clock.t.Add(31*24*time.Hour)) + `"}`, "Expiry must be between 60 seconds and 30 days."},
		{org.ID, `{"expires_at": "` + day + `", "max_uses": 0}`, "Max uses must be at least 1."},
		{p.ID, `{"expires_at": "` + day + `"}`, "Choose a role."},
		{p.ID, `{"expires_at": "` + day + `", "role": "viewer", "grants": [{"node": "` + p.ID + `", "role": "viewer"}]}`, "Only an invite to the organization can grant more."},
		{org.ID, `{"expires_at": "` + day + `", "role": "support"}`, "This role can be given for at most 7d."},
		{org.ID, `{"expires_at": "` + day + `", "role": "support", "role_duration": "8d"}`, "This role can be given for at most 7d."},
		{org.ID, `{"expires_at": "` + day + `", "role_duration": "soon"}`, "That doesn't look right."},
		{org.ID, `{"expires_at": "` + day + `", "grants": [{"node": "` + otherProject.ID + `", "role": "viewer"}]}`, "That doesn't look right."},
		{org.ID, `{"expires_at": "` + day + `", "grants": [{"node": "` + org.ID + `", "role": "owner"}]}`, "That doesn't look right."},
		{org.ID, `{"expires_at": "` + day + `", "grants": [{"node": "` + p.ID + `", "role": "member"}]}`, "That role can't be given on a project."},
		{e.signIn.ID, `{"expires_at": "` + day + `", "role": "owner"}`, "That doesn't look right."},
	} {
		r := e.call("POST", "/nodes/"+c.node+"/invites", "", c.body)
		if r.status != 422 || r.message() != c.msg {
			t.Fatalf("%s: %d %q", c.body, r.status, r.message())
		}
	}
	e.expect(e.call("POST", "/nodes/"+org.ID+"/invites", "", `{"expires_at": "`+day+`", "role": "support", "role_duration": "7d"}`), 201, "support within max")
}

func (e *env) invite(nodeID, body string) (id, token string) {
	e.t.Helper()
	r := e.call("POST", "/nodes/"+nodeID+"/invites", "", body)
	e.expect(r, 201, "invite")
	return r.str("id"), r.str("token")
}

func (e *env) redeem(dirMember, token string) response {
	e.t.Helper()
	return e.call("POST", "/members/"+dirMember+"/invites/redeem", "", `{"token": "`+token+`"}`)
}

func TestRedeemAtARoot(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	p := e.child(org, "project", "One")
	q := e.child(org, "project", "Two")
	day := rfc(e.clock.t.Add(24 * time.Hour))
	_, token := e.invite(org.ID, `{"expires_at": "`+day+`", "max_uses": 2, "role": "developer", "role_duration": "2h",
		"grants": [{"node": "`+p.ID+`", "role": "viewer", "duration": "1h"}, {"node": "`+q.ID+`", "role": "owner"}]}`)

	// A newcomer: a member with the role at the root, plus each grant on a live node.
	if _, err := e.nodes.Delete(e.ctx, e.snap(), q.ID); err != nil {
		t.Fatal(err)
	}
	frank := e.person("frank")
	before := e.lastEvent()
	r := e.redeem(frank, token)
	e.expect(r, 200, "redeem")
	member := r.str("member_id")
	if r.str("root_id") != org.ID || r.str("node_id") != org.ID {
		t.Fatalf("redeem %v", r.body)
	}
	if got := e.eventsAfter(before); !slices.Equal(got, []string{"member.created " + member + " " + org.ID + " person"}) {
		t.Fatalf("events %v", got)
	}
	if r.headers[auditx.RootHeader] != org.ID || r.headers[auditx.MemberHeader] != member {
		t.Fatalf("audit root %v", r.headers)
	}
	var rootExpiry, grantExpiry time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT expires_at FROM bindings WHERE member_id = $1 AND node_id = $2 AND role = 'developer'`, member, org.ID).Scan(&rootExpiry); err != nil {
		t.Fatalf("root binding: %v", err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT expires_at FROM bindings WHERE member_id = $1 AND node_id = $2 AND role = 'viewer'`, member, p.ID).Scan(&grantExpiry); err != nil {
		t.Fatalf("grant binding: %v", err)
	}
	if !rootExpiry.Equal(e.clock.t.Add(2*time.Hour)) || !grantExpiry.Equal(e.clock.t.Add(time.Hour)) {
		t.Fatalf("durations from redeem: %v %v", rootExpiry, grantExpiry)
	}
	if e.count(`SELECT count(*) FROM bindings WHERE node_id = $1`, q.ID) != 0 {
		t.Fatal("a grant on a deleted node should be skipped")
	}

	// Already bound at the root: a use, and nothing else, grants included.
	e.exec(`DELETE FROM bindings WHERE member_id = $1 AND node_id = $2`, member, p.ID)
	before = e.lastEvent()
	r = e.redeem(frank, token)
	e.expect(r, 200, "redeem again")
	if r.str("member_id") != member || len(e.eventsAfter(before)) != 0 || e.count(`SELECT count(*) FROM bindings WHERE member_id = $1`, member) != 1 {
		t.Fatal("a second redeem changed something")
	}

	// Two uses of two: redeemed.
	r = e.redeem(e.person("gina"), token)
	if r.status != 409 || r.message() != "This invite has already been used." || r.body["error"].(map[string]any)["details"].(map[string]any)["value"] != "redeemed" {
		t.Fatalf("used up: %d %v", r.status, r.body)
	}
	e.expect(e.redeem(frank, "inv_nope"), 404, "unknown token")
	e.expect(e.redeem(member, token), 404, "redeem as an org member")
}

func TestRedeemBelowARootMakesAGuest(t *testing.T) {
	e := setup(t)
	org, owner := e.org(e.person("dana"), "Gruow")
	p := e.child(org, "project", "One")
	day := rfc(e.clock.t.Add(24 * time.Hour))
	_, token := e.invite(p.ID, `{"expires_at": "`+day+`", "role": "viewer", "max_uses": null}`)

	outsider := e.person("outside")
	r := e.redeem(outsider, token)
	e.expect(r, 200, "redeem")
	guest := r.str("member_id")
	if r.str("node_id") != p.ID || r.str("root_id") != org.ID {
		t.Fatalf("redeem %v", r.body)
	}
	m := e.call("GET", "/nodes/"+org.ID+"/members/"+guest, "", "")
	if m.body["guest"] != true {
		t.Fatalf("not a guest: %v", m.body)
	}
	// Someone already in the tree just gets the binding.
	r = e.redeem(e.dirMemberOf(owner), token)
	if r.status != 200 || r.str("member_id") != owner {
		t.Fatalf("member redeem %d %v", r.status, r.body)
	}
	if e.count(`SELECT count(*) FROM bindings WHERE member_id = $1 AND node_id = $2`, owner, p.ID) != 1 {
		t.Fatal("member not bound")
	}
}

// dirMemberOf is the directory member of a tree member's person.
func (e *env) dirMemberOf(memberID string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT d.id FROM members d JOIN members m ON m.principal_id = d.principal_id
		WHERE m.id = $1 AND d.root_id = $2 AND d.removed_at IS NULL`, memberID, e.signIn.ID).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestInviteEndings(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	p := e.child(org, "project", "One")
	frank := e.person("frank")

	// Revoke is idempotent; a revoked token can't be redeemed.
	inviteID, token := e.invite(org.ID, `{"expires_at": "`+rfc(e.clock.t.Add(time.Hour))+`"}`)
	e.expect(e.call("DELETE", "/nodes/"+org.ID+"/invites/"+inviteID, "", ""), 204, "revoke")
	r := e.call("DELETE", "/nodes/"+org.ID+"/invites/"+inviteID, "", "")
	if r.status != 204 || r.headers[auditx.Header] != "" {
		t.Fatal("a second revoke changed something")
	}
	if r := e.redeem(frank, token); r.status != 409 || r.message() != "This invite is no longer valid." {
		t.Fatalf("revoked: %d %s", r.status, r.message())
	}
	e.expect(e.call("DELETE", "/nodes/"+p.ID+"/invites/"+inviteID, "", ""), 404, "revoke on another node")

	// Expired: read as expired at once; the sweeper writes it.
	inviteID, token = e.invite(org.ID, `{"expires_at": "`+rfc(e.clock.t.Add(2*time.Minute))+`"}`)
	e.clock.advance(3 * time.Minute)
	if r := e.redeem(frank, token); r.status != 409 || r.message() != "This invite has expired." {
		t.Fatalf("expired: %d %s", r.status, r.message())
	}
	if r := e.call("GET", "/nodes/"+org.ID+"/invites", "", ""); r.list("invites")[1]["status"] != "expired" {
		t.Fatalf("listed %v", r.list("invites"))
	}
	if _, err := e.store.ExpireDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM invites WHERE id = $1 AND status = 'expired'`, inviteID) != 1 {
		t.Fatal("sweeper didn't mark it expired")
	}

	// A node's delete revokes its invites and those below; restore leaves them revoked.
	_, rootToken := e.invite(org.ID, `{"expires_at": "`+rfc(e.clock.t.Add(time.Hour))+`"}`)
	_, projectToken := e.invite(p.ID, `{"expires_at": "`+rfc(e.clock.t.Add(time.Hour))+`", "role": "viewer"}`)
	if _, err := e.nodes.Delete(e.ctx, e.snap(), org.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.nodes.Restore(e.ctx, e.snap(), org.ID); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{rootToken, projectToken} {
		if r := e.redeem(frank, tok); r.status != 409 || r.message() != "This invite is no longer valid." {
			t.Fatalf("after delete and restore: %d %s", r.status, r.message())
		}
	}
}

func TestExpirySweep(t *testing.T) {
	e := setup(t)
	org, _ := e.org(e.person("dana"), "Gruow")
	p := e.child(org, "project", "One")
	soon := ptr(e.clock.t.Add(time.Hour))

	// A guest whose only binding expires is removed with it.
	guest := e.join(org, "guest", "member")
	e.bind(p, guest, "viewer", soon)
	e.call("DELETE", "/nodes/"+org.ID+"/bindings/"+e.bindingOf(guest, org.ID), "", "")
	// A member with a binding left keeps their member.
	kept := e.join(org, "kept", "member")
	e.bind(p, kept, "viewer", soon)
	// A service account keeps its member with no bindings.
	sa := e.call("POST", "/nodes/"+p.ID+"/service-accounts", "", `{"name": "ci", "role": "viewer", "expires_at": "`+rfc(*soon)+`"}`).str("id")

	if n, err := e.store.ExpireDue(e.ctx); err != nil || n != 0 {
		t.Fatalf("nothing due yet: %d %v", n, err)
	}
	e.clock.advance(2 * time.Hour)
	before := e.lastEvent()
	n, err := e.store.ExpireDue(e.ctx)
	if err != nil || n != 3 {
		t.Fatalf("expired %d %v", n, err)
	}
	if got := e.eventsAfter(before); !slices.Equal(got, []string{"member.removed " + guest + " " + org.ID + " person"}) {
		t.Fatalf("events %v", got)
	}
	if !e.removed(guest) || e.removed(kept) || e.removed(sa) {
		t.Fatal("wrong members removed")
	}
	if e.count(`SELECT count(*) FROM bindings WHERE expires_at IS NOT NULL`) != 0 {
		t.Fatal("expired bindings left")
	}
}
