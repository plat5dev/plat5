package members

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestTier1MemberSuspensionBindingsKeysAndTiers(t *testing.T) {
	e := setup(t)
	alice := e.person("alice")
	org, _ := e.org(alice, "Acme")
	f := e.child(org, "folder", "Apps")
	p := e.child(f, "project", "Web")
	other, _ := e.org(alice, "Other")

	// 1. Reversible per-org member suspension.
	bob := e.join(org, "bob", "developer")
	bobWho := `"idp": {"issuer": "https://idp.test", "subject": "bob"}`
	a := e.admit(admitBody(bobWho, false, "organization", org.ID, "folder", f.ID, "project", p.ID))
	if a.body["member_id"] != bob {
		t.Fatalf("expected bob admitted before suspend, got %v", a.body)
	}
	r := e.call("PATCH", "/nodes/"+org.ID+"/members/"+bob+"/status", "", `{"status":"suspended"}`)
	e.expect(r, 200, "suspend member")
	if r.str("status") != StatusSuspended {
		t.Fatalf("status = %q, want suspended", r.str("status"))
	}
	a = e.admit(admitBody(bobWho, false, "organization", org.ID, "folder", f.ID, "project", p.ID))
	if a.body["member_id"] != nil {
		t.Fatalf("expected suspended member refused at admit, got %v", a.body)
	}
	r = e.call("GET", "/nodes/"+p.ID+"/memberships/"+bob, "", "")
	e.expect(r, 200, "suspended membership")
	if len(permissionsOf(r)) != 0 {
		t.Fatalf("expected no permissions while suspended, got %v", permissionsOf(r))
	}
	r = e.call("PATCH", "/nodes/"+org.ID+"/members/"+bob+"/status", "", `{"status":"active"}`)
	e.expect(r, 200, "reactivate member")
	if r.str("status") != StatusActive || r.str("id") != bob {
		t.Fatalf("reactivated member %v", r.body)
	}
	a = e.admit(admitBody(bobWho, false, "organization", org.ID, "folder", f.ID, "project", p.ID))
	if a.body["member_id"] != bob {
		t.Fatalf("expected reactivated bob admitted with same member_id, got %v", a.body)
	}

	// 2. Expiring JIT binding + multiple distinct roles on the same node.
	carol := e.join(org, "carol", "nobody")
	r = e.call("POST", "/nodes/"+p.ID+"/bindings", "", fmt.Sprintf(`{"member_id":%q,"role":"viewer","expires_in":"1h"}`, carol))
	e.expect(r, 201, "create expiring binding")
	expBindID := r.str("id")
	if r.body["expires_at"] == nil {
		t.Fatalf("expected expires_at set, got %v", r.body)
	}
	r = e.call("POST", "/nodes/"+p.ID+"/bindings", "", fmt.Sprintf(`{"member_id":%q,"role":"developer","expires_in":"2h"}`, carol))
	e.expect(r, 201, "second distinct role on same node")
	carolWho := `"idp": {"issuer": "https://idp.test", "subject": "carol"}`
	a = e.admit(admitBody(carolWho, false, "organization", org.ID, "folder", f.ID, "project", p.ID))
	if !slices.Contains(permissionsOf(a), "project:write") || a.body["expires_at"] == nil {
		t.Fatalf("expected project:write + expires_at, got %v", a.body)
	}
	// Advance past both expirations: member stays alive, permissions drop to empty.
	e.clock.advance(3 * time.Hour)
	a = e.admit(admitBody(carolWho, false, "organization", org.ID, "folder", f.ID, "project", p.ID))
	if a.body["member_id"] != carol || len(permissionsOf(a)) != 0 {
		t.Fatalf("expected member alive with 0 permissions after expiry, got %v", a.body)
	}
	// Clear expiry via PATCH -> permissions come back.
	r = e.call("PATCH", "/nodes/"+p.ID+"/bindings/"+expBindID, "", `{"expires_at":null}`)
	e.expect(r, 200, "clear binding expiry")
	a = e.admit(admitBody(carolWho, false, "organization", org.ID, "folder", f.ID, "project", p.ID))
	if !slices.Equal(permissionsOf(a), []string{"project:read"}) {
		t.Fatalf("expected project:read after clearing expiry, got %v", permissionsOf(a))
	}

	// 3. Scoped personal API key with allowed_roots and TTL.
	r = e.call("POST", "/users/"+alice+"/keys", "", fmt.Sprintf(`{"name":"narrow","scopes":["project:read"],"allowed_roots":[%q],"expires_in":"1h"}`, org.ID))
	e.expect(r, 201, "create scoped key")
	narrowKey := r.str("key")
	// Allowed in org with narrowed permissions (admin "*" -> ["project:read"]).
	a = e.admit(admitBody(fmt.Sprintf(`"credential":%q`, narrowKey), false, "organization", org.ID, "folder", f.ID, "project", p.ID))
	if a.body["valid"] != true || !slices.Equal(permissionsOf(a), []string{"project:read"}) || a.body["expires_at"] == nil {
		t.Fatalf("scoped key admit in allowed org: %v", a.body)
	}
	// Blocked in other org (member_id: null).
	a = e.admit(admitBody(fmt.Sprintf(`"credential":%q`, narrowKey), false, "organization", other.ID))
	if a.body["valid"] != true || a.body["member_id"] != nil {
		t.Fatalf("scoped key should be denied in disallowed org: %v", a.body)
	}
	// Blocked on user route (user_id: null).
	a = e.admit(admitBody(fmt.Sprintf(`"credential":%q`, narrowKey), false))
	if a.body["valid"] != true || a.body["user_id"] != nil {
		t.Fatalf("scoped key must not get user_id on user route: %v", a.body)
	}
	// Expired key -> valid: false.
	e.clock.advance(2 * time.Hour)
	a = e.admit(admitBody(fmt.Sprintf(`"credential":%q`, narrowKey), false, "organization", org.ID))
	if a.body["valid"] != false {
		t.Fatalf("expired key should be valid: false, got %v", a.body)
	}

	// 4. Root rate_limit_tier.
	r = e.call("PATCH", "/nodes/"+p.ID+"/rate-limit-tier", "", `{"rate_limit_tier":"pro"}`)
	e.expect(r, 422, "tier on non-root rejected")
	r = e.call("PATCH", "/nodes/"+org.ID+"/rate-limit-tier", "", `{"rate_limit_tier":"enterprise"}`)
	e.expect(r, 200, "set root tier")
	if r.str("rate_limit_tier") != "enterprise" {
		t.Fatalf("node rate_limit_tier = %v", r.body["rate_limit_tier"])
	}
	a = e.admit(admitBody(bobWho, false, "organization", org.ID, "folder", f.ID, "project", p.ID))
	if a.body["rate_limit_tier"] != "enterprise" {
		t.Fatalf("admit rate_limit_tier = %v, want enterprise", a.body["rate_limit_tier"])
	}
	r = e.call("PATCH", "/nodes/"+org.ID+"/rate-limit-tier", "", `{"rate_limit_tier":null}`)
	e.expect(r, 200, "clear root tier")
	if r.body["rate_limit_tier"] != nil {
		t.Fatalf("expected cleared tier, got %v", r.body["rate_limit_tier"])
	}
}
