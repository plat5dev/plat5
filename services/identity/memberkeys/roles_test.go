package memberkeys

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/plat5dev/plat5/identity/orgs"
	"github.com/plat5dev/plat5/identity/roles"
)

const starterRoles = `
roles:
  owner: ["*"]
  admin: [org:write, org:members:write, org:service-accounts:write]
  member: []
creator_role: owner
default_role: member
`

func starterSet(t *testing.T) *roles.Set {
	t.Helper()
	s, err := roles.Parse([]byte(starterRoles))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rolePtr(s string) *string { return &s }

func TestValidateReturnsRoleLabels(t *testing.T) {
	cases := []struct {
		name  string
		roles *roles.Set
		role  *string
		want  any
	}{
		{"owner", starterSet(t), rolePtr("owner"), []any{"*"}},
		{"admin", starterSet(t), rolePtr("admin"), []any{"org:write", "org:members:write", "org:service-accounts:write"}},
		{"member", starterSet(t), rolePtr("member"), []any{}},
		{"role removed from the file", starterSet(t), rolePtr("gone"), []any{}},
		{"roles off", nil, nil, []any{"*"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := &fakeKeys{validated: &Validated{
				Key:            &APIKey{ID: "k1", MemberID: "mem1"},
				OrganizationID: "org1",
				MemberStatus:   string(orgs.StatusActive),
				MemberRole:     tc.role,
			}}
			h := &Handler{store: keys, orgStore: &fakeOrgs{}, prefix: testPrefix, roles: tc.roles}
			app := testKeyApp(h)
			h.MountInternal(app.Group("/internal"))

			code, body := doJSON(t, app, http.MethodPost, "/internal/member-keys/validate", `{"key":"`+testPrefix+`abcdefabcdefabcdefabcdefabcdefab"}`)
			if code != http.StatusOK {
				t.Fatalf("validate: %d %s", code, body)
			}
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatal(err)
			}
			if raw["valid"] != true {
				t.Fatalf("not valid: %s", body)
			}
			if _, ok := raw["role"]; ok {
				t.Fatalf("validate must not return the role: %s", body)
			}
			if !reflect.DeepEqual(raw["labels"], tc.want) {
				t.Fatalf("labels %#v, want %#v", raw["labels"], tc.want)
			}
		})
	}
}

// A service account is the org's, not the caller's. Whoever may call the route
// mints and revokes its keys, whatever its role.
func TestServiceAccountKeysForAnyRole(t *testing.T) {
	keys := &fakeKeys{}
	org := &fakeOrgs{}
	ownerSA := saFixture("org1", "sa-owner", "mem-owner", orgs.StatusActive)
	ownerSA.Role = rolePtr("owner")
	org.add(ownerSA)
	h := &Handler{store: keys, orgStore: org, prefix: testPrefix, roles: starterSet(t)}
	app := testKeyApp(h)
	code, body := doJSON(t, app, http.MethodPost, "/organizations/org1/service-accounts/sa-owner/api-keys", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("mint for an owner SA: %d %s", code, body)
	}
	created := decodeCreate(t, body)

	code, body = doJSON(t, app, http.MethodDelete, "/organizations/org1/service-accounts/sa-owner/api-keys/"+created.ID, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke for an owner SA: %d %s", code, body)
	}
}
