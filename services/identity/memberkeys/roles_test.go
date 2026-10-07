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

func TestValidateReturnsEffectiveScopes(t *testing.T) {
	cases := []struct {
		name      string
		role      *string
		keyScopes []string
		want      any
	}{
		{"owner, unrestricted key", rolePtr("owner"), nil, nil},
		{"owner, narrowed key", rolePtr("owner"), []string{"x"}, []any{"x"}},
		{"admin, unrestricted key", rolePtr("admin"), nil, []any{"org:write", "org:members:write", "org:service-accounts:write"}},
		{"admin, narrowed key", rolePtr("admin"), []string{"org:write", "x"}, []any{"org:write"}},
		{"member", rolePtr("member"), nil, []any{}},
		{"role removed from the file", rolePtr("gone"), nil, []any{}},
		{"null role", nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := &fakeKeys{validated: &Validated{
				Key:            &APIKey{ID: "k1", MemberID: "mem1", Scopes: tc.keyScopes},
				OrganizationID: "org1",
				MemberStatus:   string(orgs.StatusActive),
				MemberRole:     tc.role,
			}}
			h := &Handler{store: keys, orgStore: &fakeOrgs{}, prefix: testPrefix, roles: starterSet(t)}
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
			if !reflect.DeepEqual(raw["scopes"], tc.want) {
				t.Fatalf("scopes %#v, want %#v", raw["scopes"], tc.want)
			}
		})
	}
}

func TestServiceAccountKeysCapOnRole(t *testing.T) {
	keys := &fakeKeys{}
	org := &fakeOrgs{}
	ownerSA := saFixture("org1", "sa-owner", "mem-owner", orgs.StatusActive)
	ownerSA.Role = rolePtr("owner")
	memberSA := saFixture("org1", "sa-member", "mem-member", orgs.StatusActive)
	memberSA.Role = rolePtr("member")
	org.add(ownerSA)
	org.add(memberSA)
	h := &Handler{store: keys, orgStore: org, prefix: testPrefix, roles: starterSet(t)}
	app := testKeyApp(h)
	admin := map[string]string{"X-Plat5-Scopes": "org:write,org:members:write,org:service-accounts:write"}

	code, body := doJSONHeader(t, app, http.MethodPost, "/organizations/org1/service-accounts/sa-owner/api-keys", `{}`, admin)
	assertRoleCap(t, code, body, "You can't change a member with the owner role.")

	code, body = doJSONHeader(t, app, http.MethodDelete, "/organizations/org1/service-accounts/sa-owner/api-keys/k1", "", admin)
	assertRoleCap(t, code, body, "You can't change a member with the owner role.")

	code, body = doJSONHeader(t, app, http.MethodGet, "/organizations/org1/service-accounts/sa-owner/api-keys", "", admin)
	if code != http.StatusOK {
		t.Fatalf("listing is a read, not capped: %d %s", code, body)
	}

	code, body = doJSONHeader(t, app, http.MethodPost, "/organizations/org1/service-accounts/sa-member/api-keys", `{}`, admin)
	if code != http.StatusCreated {
		t.Fatalf("admin mints for a member-role SA: %d %s", code, body)
	}

	code, body = doJSON(t, app, http.MethodPost, "/organizations/org1/service-accounts/sa-owner/api-keys", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("unrestricted caller mints for an owner SA: %d %s", code, body)
	}
}

func assertRoleCap(t *testing.T, code int, body []byte, message string) {
	t.Helper()
	if code != http.StatusForbidden {
		t.Fatalf("want 403, got %d %s", code, body)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "INSUFFICIENT_SCOPE" || env.Error.Message != message {
		t.Fatalf("got %s", body)
	}
}
