package orgs

import (
	"encoding/json"
	stderrors "errors"
	"net/http"
	"testing"

	apierrors "github.com/plat5dev/plat5/identity/errors"
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

func strPtrOf(s string) *string { return &s }

func TestInviteRoleDefaultsAndRedeemAssignsIt(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	app := testInviteApp(&Handler{invites: f, roles: starterSet(t)})

	code, body := doJSON(t, app, http.MethodPost, "/organizations/org1/invites", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var inv InviteResponse
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Role == nil || *inv.Role != "member" {
		t.Fatalf("omitted role is default_role: %s", body)
	}

	code, body = doJSON(t, app, http.MethodPost, "/users/new1/invites/redeem", `{"token":"`+inv.Token+`"}`)
	if code != http.StatusOK {
		t.Fatalf("redeem: %d %s", code, body)
	}
	var mem MemberResponse
	if err := json.Unmarshal(body, &mem); err != nil {
		t.Fatal(err)
	}
	if mem.Role == nil || *mem.Role != "member" {
		t.Fatalf("redeem assigns the invite's role: %s", body)
	}
}

// No grant cap: whoever may call the route assigns any role.
func TestInviteAssignsAnyRole(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	app := testInviteApp(&Handler{invites: f, roles: starterSet(t)})

	code, body := doJSON(t, app, http.MethodPost, "/organizations/org1/invites", `{"role":"owner"}`)
	if code != http.StatusCreated {
		t.Fatalf("invite an owner: %d %s", code, body)
	}
	var inv InviteResponse
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Role == nil || *inv.Role != "owner" {
		t.Fatalf("role: %s", body)
	}
}

func TestInviteRoleValidation(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")

	app := testInviteApp(&Handler{invites: f, roles: starterSet(t)})
	code, body := doJSON(t, app, http.MethodPost, "/organizations/org1/invites", `{"role":"nope"}`)
	assertFieldError(t, code, body, "role", "That role doesn't exist.")

	noRoles := testInviteApp(&Handler{invites: f})
	code, body = doJSON(t, noRoles, http.MethodPost, "/organizations/org1/invites", `{"role":"admin"}`)
	assertFieldError(t, code, body, "role", "Roles aren't set up for this deployment.")

	code, body = doJSON(t, noRoles, http.MethodPost, "/organizations/org1/invites", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("no roles file, no role: %d %s", code, body)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw["role"]; !ok || v != nil {
		t.Fatalf("role is null without a roles file: %s", body)
	}
}

func TestRejectLastCreator(t *testing.T) {
	set := starterSet(t)
	member := func(id, role string, status Status) *Member {
		return &Member{ID: id, Role: strPtrOf(role), Status: status}
	}

	t.Run("demoting the only owner", func(t *testing.T) {
		owner := member("m1", "owner", StatusActive)
		members := []*Member{owner, member("m2", "admin", StatusActive)}
		prior := *owner
		owner.Role = strPtrOf("admin")
		assertLastCreator(t, rejectLastCreator(set, members, prior, "role"), "role")
	})
	t.Run("removing the only owner", func(t *testing.T) {
		owner := member("m1", "owner", StatusActive)
		members := []*Member{owner, member("m2", "admin", StatusActive)}
		prior := *owner
		owner.Status = StatusRemoved
		err := rejectLastCreator(set, members, prior, "member_id")
		assertLastCreator(t, err, "member_id")
	})
	t.Run("suspended owner still counts", func(t *testing.T) {
		owner := member("m1", "owner", StatusActive)
		members := []*Member{owner, member("m2", "owner", StatusSuspended)}
		prior := *owner
		owner.Status = StatusRemoved
		if err := rejectLastCreator(set, members, prior, "member_id"); err != nil {
			t.Fatalf("a suspended owner remains: %v", err)
		}
	})
	t.Run("removing a non-owner", func(t *testing.T) {
		admin := member("m2", "admin", StatusActive)
		members := []*Member{member("m1", "owner", StatusActive), admin}
		prior := *admin
		admin.Status = StatusRemoved
		if err := rejectLastCreator(set, members, prior, "member_id"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("org with no owner already", func(t *testing.T) {
		legacy := &Member{ID: "m1", Status: StatusActive}
		members := []*Member{legacy, member("m2", "admin", StatusActive)}
		prior := *legacy
		legacy.Status = StatusRemoved
		if err := rejectLastCreator(set, members, prior, "member_id"); err != nil {
			t.Fatalf("nothing to keep: %v", err)
		}
	})
	t.Run("no roles file", func(t *testing.T) {
		owner := member("m1", "owner", StatusActive)
		prior := *owner
		owner.Status = StatusRemoved
		if err := rejectLastCreator(nil, []*Member{owner}, prior, "member_id"); err != nil {
			t.Fatal(err)
		}
	})
}

func assertLastCreator(t *testing.T, err error, path string) {
	t.Helper()
	var apiErr *apierrors.ApiError
	if !stderrors.As(err, &apiErr) || apiErr.Code != "VALIDATION_ERROR" {
		t.Fatalf("want 422, got %v", err)
	}
	if apiErr.Message != "Keep at least one member with the owner role." {
		t.Fatalf("message %q", apiErr.Message)
	}
	fields := apiErr.Details.(map[string]any)["fields"].([]apierrors.Field)
	if len(fields) != 1 || fields[0].Path != path {
		t.Fatalf("fields %+v, want path %q", fields, path)
	}
}

func assertFieldError(t *testing.T, code int, body []byte, path, message string) {
	t.Helper()
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d %s", code, body)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Details struct {
				Fields []struct {
					Path string `json:"path"`
				} `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Message != message || len(env.Error.Details.Fields) != 1 || env.Error.Details.Fields[0].Path != path {
		t.Fatalf("got %s", body)
	}
}
