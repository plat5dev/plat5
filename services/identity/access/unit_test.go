package access

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/id"
)

const unitTestConfig = `{
	"revision": 1, "deletion_grace": "30d", "reserved_slugs": ["new"],
	"kinds": {
		"organization": { "path": "/orgs/{organization}", "list_permission": "organization:read" },
		"folder": { "parent": "organization", "path": "/folders/{folder}", "list_permission": "folder:read" },
		"project": { "parent": "folder", "path": "/projects/{project}", "list_permission": "project:read" }
	},
	"roles": {
		"admin": { "permissions": ["*"], "bindable_at": ["organization", "folder", "project"] },
		"member": { "permissions": ["organization:read"], "bindable_at": ["organization"] },
		"developer": { "includes": ["member"], "permissions": ["project:read", "project:write"], "bindable_at": ["organization", "folder", "project"] },
		"viewer": { "permissions": ["project:read"], "bindable_at": ["project"] },
		"browser": { "permissions": ["folder:read"], "bindable_at": ["organization", "folder"] },
		"nobody": { "permissions": [], "bindable_at": ["organization"] },
		"gone_here": { "permissions": ["project:read"], "bindable_at": ["organization"] }
	},
	"creator_role": "admin"
}`

func parseUnitConfig(t *testing.T, body string) *feed.Snapshot {
	t.Helper()
	s, err := feed.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResolve(t *testing.T) {
	snap := parseUnitConfig(t, unitTestConfig)

	p := Resolve(snap, []string{"viewer", "browser", "nonexistent"})
	if p.All || !p.Has("project:read") || !p.Has("folder:read") || p.Has("project:write") {
		t.Fatalf("unexpected resolved permissions: %+v", p)
	}
	if got := p.List(); !slices.Equal(got, []string{"folder:read", "project:read"}) {
		t.Fatalf("List() = %v", got)
	}

	admin := Resolve(snap, []string{"admin"})
	if !admin.All || !admin.Has("anything:at-all") {
		t.Fatalf("admin should match all permissions: %+v", admin)
	}

	// IntersectScopes narrows both concrete permissions and "*".
	if got := IntersectScopes(p, []string{"project:read", "project:write", "project:read"}); !slices.Equal(got, []string{"project:read"}) {
		t.Fatalf("IntersectScopes(p) = %v", got)
	}
	if got := IntersectScopes(admin, []string{"project:write", "project:read", "*"}); !slices.Equal(got, []string{"project:read", "project:write"}) {
		t.Fatalf("IntersectScopes(admin) = %v", got)
	}
	if got := IntersectScopes(admin, []string{}); len(got) != 0 {
		t.Fatalf("IntersectScopes(admin, empty) = %v", got)
	}

	// ResolveWithCustom ignores custom permissions when custom_roles is false.
	disabled := ResolveWithCustom(snap, []string{"viewer"}, []string{"billing:read", "*", "project:read"})
	if disabled.All || disabled.Has("billing:read") || !disabled.Has("project:read") {
		t.Fatalf("ResolveWithCustom (disabled) = %+v", disabled)
	}

	// ResolveWithCustom unions custom permissions and ignores "*" when custom_roles is true.
	snapWithCustom := parseUnitConfig(t, strings.Replace(unitTestConfig, `"creator_role": "admin"`, `"creator_role": "admin", "custom_roles": true`, 1))
	custom := ResolveWithCustom(snapWithCustom, []string{"viewer"}, []string{"billing:read", "*", "project:read"})
	if custom.All || !custom.Has("billing:read") || !custom.Has("project:read") || custom.Has("other:perm") {
		t.Fatalf("ResolveWithCustom (enabled) = %+v", custom)
	}
}

func TestMatchesRequiredClaims(t *testing.T) {
	req := map[string][]string{
		"amr":            {"mfa", "hwk"},
		"idp_connection": {"okta-acme"},
	}
	if !matchesRequiredClaims(map[string][]string{"amr": {"pwd", "mfa"}, "idp_connection": {"okta-acme"}}, req) {
		t.Fatal("expected matching claims to pass")
	}
	if matchesRequiredClaims(map[string][]string{"amr": {"pwd"}, "idp_connection": {"okta-acme"}}, req) {
		t.Fatal("expected missing amr value to fail")
	}
	if matchesRequiredClaims(map[string][]string{"amr": {"mfa"}}, req) {
		t.Fatal("expected missing idp_connection to fail")
	}
	if !matchesRequiredClaims(nil, nil) {
		t.Fatal("expected empty required claims to pass")
	}
}

func TestCheckAdmit(t *testing.T) {
	if err := checkAdmit(AdmitInput{Chain: []ChainRef{{Kind: "organization", Ref: "acme"}}}); err == nil {
		t.Fatal("expected missing credential and idp to fail")
	}
	cred := "k"
	if err := checkAdmit(AdmitInput{Credential: &cred, IdP: &IdPClaims{Issuer: "https://idp.test", Subject: "s"}}); err == nil {
		t.Fatal("expected credential + idp to fail")
	}
	if err := checkAdmit(AdmitInput{Credential: &cred, Chain: []ChainRef{{Kind: "organization", Ref: "acme"}}}); err != nil {
		t.Fatalf("expected credential kind admit valid, got %v", err)
	}
}

func TestKindAtOrBelow(t *testing.T) {
	snap := parseUnitConfig(t, unitTestConfig)
	cases := []struct {
		kind, scope string
		want        bool
	}{
		{"organization", "organization", true},
		{"folder", "organization", true},
		{"project", "organization", true},
		{"project", "folder", true},
		{"project", "project", true},
		{"organization", "folder", false},
		{"folder", "project", false},
		{"unknown", "organization", false},
	}
	for _, tc := range cases {
		if got := kindAtOrBelow(snap, tc.kind, tc.scope); got != tc.want {
			t.Errorf("kindAtOrBelow(%q, %q) = %v, want %v", tc.kind, tc.scope, got, tc.want)
		}
	}
}

func TestGrantingPairs(t *testing.T) {
	snap := parseUnitConfig(t, unitTestConfig)
	kinds, roles := snap.GrantingPairs("project:write")
	var pairs []string
	for i := range kinds {
		pairs = append(pairs, kinds[i]+":"+roles[i])
	}
	slices.Sort(pairs)
	want := []string{
		"folder:admin", "folder:developer",
		"organization:admin", "organization:developer",
		"project:admin", "project:developer",
	}
	if !slices.Equal(pairs, want) {
		t.Fatalf("GrantingPairs(project:write) = %v, want %v", pairs, want)
	}

	// A permission granted only by "*".
	kinds, roles = snap.GrantingPairs("billing:export")
	if len(kinds) != 3 {
		t.Fatalf("wildcard-only pairs: %v %v", kinds, roles)
	}
	for _, r := range roles {
		if r != "admin" {
			t.Fatalf("expected only admin, got %v", roles)
		}
	}
}

func TestReadResourceSearchValidation(t *testing.T) {
	uid := id.New()
	good := func(body string) ResourceSearch {
		t.Helper()
		obj, err := readObject([]byte(body))
		if err != nil {
			t.Fatalf("readObject: %v", err)
		}
		in, err := readResourceSearch(obj)
		if err != nil {
			t.Fatalf("readResourceSearch(%s): %v", body, err)
		}
		return in
	}
	in := good(fmt.Sprintf(`{
		"subject": {"type": "member", "id": %q},
		"action": {"name": "project:read"},
		"resource": {"type": "project", "id": "ignored"},
		"scope": {"node_id": %q},
		"page": {"limit": 25, "token": %q},
		"context": {"extra": true}
	}`, uid, uid, uid))
	if in.MemberID != uid || in.Permission != "project:read" || in.Kind != "project" ||
		in.ScopeNodeID != uid || in.Limit != 25 || in.After != uid {
		t.Fatalf("parsed %+v", in)
	}

	badCases := []struct {
		name, body, wantSubstr string
	}{
		{"missing subject", `{"action":{"name":"a"},"resource":{"type":"project"}}`, "subject"},
		{"wrong subject type", fmt.Sprintf(`{"subject":{"type":"user","id":%q},"action":{"name":"a"},"resource":{"type":"project"}}`, uid), "subject.type"},
		{"bad subject id", `{"subject":{"type":"member","id":"bad"},"action":{"name":"a"},"resource":{"type":"project"}}`, "subject.id"},
		{"missing resource", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"}}`, uid), "resource"},
		{"empty resource type", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"},"resource":{"type":""}}`, uid), "resource.type"},
		{"missing action", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"resource":{"type":"project"}}`, uid), "action"},
		{"empty action name", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":""},"resource":{"type":"project"}}`, uid), "action.name"},
		{"bad scope id", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"},"resource":{"type":"project"},"scope":{"node_id":"nope"}}`, uid), "scope.node_id"},
		{"limit zero", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"},"resource":{"type":"project"},"page":{"limit":0}}`, uid), "page.limit"},
		{"limit too high", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"},"resource":{"type":"project"},"page":{"limit":1001}}`, uid), "page.limit"},
		{"bad page token", fmt.Sprintf(`{"subject":{"type":"member","id":%q},"action":{"name":"a"},"resource":{"type":"project"},"page":{"token":"not-ulid"}}`, uid), "page.token"},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			obj, err := readObject([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			_, err = readResourceSearch(obj)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestAdmitResponseAlwaysIncludesEveryField(t *testing.T) {
	// The gateway requires every AdmitResponse field to be present on the wire,
	// even when nil (it treats a missing key as identity failing with 503).
	raw, err := json.Marshal(admitResponse(&Admitted{Valid: true, Principal: "person", UserID: "01u"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"valid",
		"principal",
		"key_id",
		"allowed_cidrs",
		"org_allowed_cidrs",
		"nodes",
		"member_id",
		"user_id",
		"permissions",
		"credential_scopes",
		"rate_limit_tier",
		"expires_at",
		"invalidation_seq",
		"config_revision",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("AdmitResponse JSON missing required key %q: %s", key, string(raw))
		}
	}
}
