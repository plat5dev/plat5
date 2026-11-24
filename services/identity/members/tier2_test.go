package members

import (
	"fmt"
	"slices"
	"testing"
)

const tier2TestConfig = `{
	"revision": 1, "deletion_grace": "30d", "reserved_slugs": ["new"],
	"kinds": {
		"organization": { "path": "/orgs/{organization}", "list_permission": "organization:read", "allow_public": true },
		"folder": { "parent": "organization", "path": "/folders/{folder}", "list_permission": "folder:read" },
		"project": { "parent": "folder", "path": "/projects/{project}", "list_permission": "project:read", "allow_public": true }
	},
	"roles": {
		"admin": { "permissions": ["*"], "bindable_at": ["organization", "folder", "project"] },
		"member": { "permissions": ["organization:read"], "bindable_at": ["organization"] },
		"developer": { "includes": ["member"], "permissions": ["project:read", "project:write"], "bindable_at": ["organization", "folder", "project"] },
		"viewer": { "permissions": ["project:read"], "bindable_at": ["project"] }
	},
	"creator_role": "admin",
	"custom_roles": true,
	"permissions": {
		"organization:read": { "description": "Read org", "bindable_at": [], "custom_role_assignable": true },
		"project:read": { "description": "Read projects", "bindable_at": [], "custom_role_assignable": true },
		"project:write": { "description": "Write projects", "bindable_at": [], "custom_role_assignable": true },
		"billing:read": { "description": "Read billing", "bindable_at": ["organization"], "custom_role_assignable": true },
		"internal:root": { "description": "Internal only", "bindable_at": [], "custom_role_assignable": false }
	}
}`

func TestTier2SecurityPoliciesCustomRolesAndPublicPrincipals(t *testing.T) {
	e := setupWith(t, tier2TestConfig)
	alice := e.person("alice")
	org, _ := e.org(alice, "Acme")
	f := e.child(org, "folder", "Apps")
	p := e.child(f, "project", "Web")
	bob := e.join(org, "bob", "member")

	// 1. Per-org auth & security policy.
	r := e.call("GET", "/nodes/"+org.ID+"/security-policy", "", "")
	e.expect(r, 200, "default security policy")
	if r.body["allow_personal_keys"] != true {
		t.Fatalf("default allow_personal_keys = %v", r.body["allow_personal_keys"])
	}
	r = e.call("PATCH", "/nodes/"+p.ID+"/security-policy", "", `{"allow_personal_keys":false}`)
	e.expect(r, 422, "security policy on non-root rejected")

	r = e.call("PATCH", "/nodes/"+org.ID+"/security-policy", "", `{
		"allow_personal_keys": false,
		"allowed_cidrs": ["10.0.0.0/8"],
		"required_idp_claims": {"amr": ["mfa"], "idp_connection": ["okta-acme"]}
	}`)
	e.expect(r, 200, "update security policy")

	// Personal API key is blocked in org, while service account key is still allowed.
	r = e.call("POST", "/users/"+alice+"/keys", "", `{"name":"personal"}`)
	e.expect(r, 201, "create personal key")
	personalKey := r.str("key")
	a := e.admit(admitBody(fmt.Sprintf(`"credential":%q`, personalKey), false, "organization", org.ID))
	if a.body["valid"] != true || a.body["member_id"] != nil {
		t.Fatalf("personal key should be blocked by allow_personal_keys=false: %v", a.body)
	}
	if cidrs, _ := a.body["org_allowed_cidrs"].([]any); len(cidrs) != 1 || cidrs[0] != "10.0.0.0/8" {
		t.Fatalf("expected org_allowed_cidrs=[10.0.0.0/8], got %v", a.body["org_allowed_cidrs"])
	}

	// JWT without required claims is blocked; JWT with matching claims is admitted.
	a = e.admit(admitBody(`"idp": {"issuer": "https://idp.test", "subject": "bob", "claims": {"amr": ["pwd"], "idp_connection": ["okta-acme"]}}`, false, "organization", org.ID))
	if a.body["member_id"] != nil {
		t.Fatalf("JWT missing mfa should be denied: %v", a.body)
	}
	a = e.admit(admitBody(`"idp": {"issuer": "https://idp.test", "subject": "bob", "claims": {"amr": ["pwd", "mfa"], "idp_connection": ["okta-acme"]}}`, false, "organization", org.ID))
	if a.body["member_id"] != bob {
		t.Fatalf("JWT with matching claims should be admitted: %v", a.body)
	}

	// Clear security policy for subsequent checks.
	r = e.call("PATCH", "/nodes/"+org.ID+"/security-policy", "", `{"allow_personal_keys":true,"allowed_cidrs":[],"required_idp_claims":{}}`)
	e.expect(r, 200, "reset security policy")

	// 2. Per-org custom roles & permissions catalog.
	r = e.call("GET", "/nodes/"+org.ID+"/permissions", "", "")
	e.expect(r, 200, "list permissions")
	if r.body["custom_roles"] != true {
		t.Fatalf("expected custom_roles=true, got %v", r.body)
	}
	// Disallowed permission rejected.
	r = e.call("POST", "/nodes/"+org.ID+"/roles", "", `{"display_name":"Bad","permissions":["internal:root"],"bindable_at":["organization"]}`)
	e.expect(r, 422, "non-assignable permission rejected")
	// Permission with bindable_at=[organization] cannot be placed on a project-bindable role.
	r = e.call("POST", "/nodes/"+org.ID+"/roles", "", `{"display_name":"BadKind","permissions":["billing:read"],"bindable_at":["project"]}`)
	e.expect(r, 422, "incompatible bindable_at rejected")

	r = e.call("POST", "/nodes/"+org.ID+"/roles", "", `{"display_name":"Billing Viewer","description":"Can view billing","permissions":["billing:read"],"bindable_at":["organization"]}`)
	e.expect(r, 201, "create custom role")
	customRoleID := r.str("id")
	if r.str("source") != RoleSourceCustom {
		t.Fatalf("expected source=custom, got %v", r.body)
	}
	// Unified roles list returns both system and custom roles.
	r = e.call("GET", "/nodes/"+org.ID+"/roles", "", "")
	e.expect(r, 200, "list unified roles")
	// Bind custom role to bob alongside existing member role (multiple bindings).
	r = e.call("POST", "/nodes/"+org.ID+"/bindings", "", fmt.Sprintf(`{"member_id":%q,"role":%q}`, bob, customRoleID))
	e.expect(r, 201, "bind custom role")

	bobWho := `"idp": {"issuer": "https://idp.test", "subject": "bob"}`
	a = e.admit(admitBody(bobWho, false, "organization", org.ID))
	if !slices.Contains(permissionsOf(a), "billing:read") || !slices.Contains(permissionsOf(a), "organization:read") {
		t.Fatalf("expected billing:read + organization:read, got %v", permissionsOf(a))
	}
	// Membership reflects custom role permissions.
	r = e.call("GET", "/nodes/"+org.ID+"/memberships/"+bob, "", "")
	e.expect(r, 200, "membership with custom role")
	if !slices.Contains(permissionsOf(r), "billing:read") {
		t.Fatalf("membership missing billing:read: %v", permissionsOf(r))
	}
	// Update custom role permissions -> reflected at admit.
	r = e.call("PATCH", "/nodes/"+org.ID+"/roles/"+customRoleID, "", `{"permissions":["billing:read","project:read"]}`)
	e.expect(r, 200, "patch custom role")
	a = e.admit(admitBody(bobWho, false, "organization", org.ID, "folder", f.ID, "project", p.ID))
	if !slices.Contains(permissionsOf(a), "project:read") {
		t.Fatalf("expected inherited project:read from patched custom role, got %v", permissionsOf(a))
	}
	// Delete custom role cleans up its bindings.
	r = e.call("DELETE", "/nodes/"+org.ID+"/roles/"+customRoleID, "", "")
	e.expect(r, 204, "delete custom role")
	a = e.admit(admitBody(bobWho, false, "organization", org.ID))
	if slices.Contains(permissionsOf(a), "billing:read") {
		t.Fatalf("deleted custom role should no longer grant billing:read: %v", permissionsOf(a))
	}

	// 3. Public principal bindings (all_users and all_authenticated_users).
	// Folder doesn't set allow_public -> 422.
	r = e.call("POST", "/nodes/"+f.ID+"/bindings", "", `{"public_principal":"all_users","role":"developer"}`)
	e.expect(r, 422, "public binding on non-allow_public kind rejected")

	// Project sets allow_public -> 201.
	r = e.call("POST", "/nodes/"+p.ID+"/bindings", "", `{"public_principal":"all_users","role":"viewer"}`)
	e.expect(r, 201, "create all_users binding")
	if r.str("public_principal") != PrincipalAllUsers {
		t.Fatalf("expected public_principal=all_users, got %v", r.body)
	}
	// Anonymous admit with allow_public=true succeeds with project:read and no member_id.
	anonBody := fmt.Sprintf(`{"anonymous":true,"allow_public":true,"chain":[{"kind":"organization","ref":%q},{"kind":"folder","ref":%q},{"kind":"project","ref":%q}]}`, org.ID, f.ID, p.ID)
	a = e.admit(anonBody)
	if a.body["valid"] != true || a.body["principal"] != "anonymous" || a.body["member_id"] != nil || !slices.Equal(permissionsOf(a), []string{"project:read"}) {
		t.Fatalf("anonymous admit: %v", a.body)
	}

	// Non-member authenticated user (stranger) is refused when allow_public=false,
	// and admitted without member_id when allow_public=true.
	strangerWho := `"idp": {"issuer": "https://idp.test", "subject": "stranger"}`
	a = e.admit(admitBody(strangerWho, false, "organization", org.ID, "folder", f.ID, "project", p.ID))
	if a.body["member_id"] != nil || len(permissionsOf(a)) != 0 {
		t.Fatalf("stranger should not be admitted without allow_public: %v", a.body)
	}
	strangerPub := fmt.Sprintf(`{%s,"allow_public":true,"chain":[{"kind":"organization","ref":%q},{"kind":"folder","ref":%q},{"kind":"project","ref":%q}]}`, strangerWho, org.ID, f.ID, p.ID)
	a = e.admit(strangerPub)
	if a.body["member_id"] != nil || !slices.Equal(permissionsOf(a), []string{"project:read"}) {
		t.Fatalf("stranger should get public project:read with allow_public=true: %v", a.body)
	}

	// Suspended member of org does NOT bypass suspension via all_users / all_authenticated_users.
	r = e.call("PATCH", "/nodes/"+org.ID+"/members/"+bob+"/status", "", `{"status":"suspended"}`)
	e.expect(r, 200, "suspend bob")
	bobPub := fmt.Sprintf(`{%s,"allow_public":true,"chain":[{"kind":"organization","ref":%q},{"kind":"folder","ref":%q},{"kind":"project","ref":%q}]}`, bobWho, org.ID, f.ID, p.ID)
	a = e.admit(bobPub)
	if a.body["member_id"] != nil || len(permissionsOf(a)) != 0 {
		t.Fatalf("suspended member should not receive public permissions in their org: %v", a.body)
	}
}
