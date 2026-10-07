package orgs

import (
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

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

const adminScopes = "org:write,org:members:write,org:service-accounts:write"

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

func TestInviteRoleGrantCap(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	app := testInviteApp(&Handler{invites: f, roles: starterSet(t)})
	admin := map[string]string{"X-Plat5-Scopes": adminScopes}

	code, body := doJSONHeaders(t, app, http.MethodPost, "/organizations/org1/invites", `{"role":"owner"}`, admin)
	assertInsufficientRole(t, code, body, "You can't assign the owner role.", []string{"*"})

	code, body = doJSONHeaders(t, app, http.MethodPost, "/organizations/org1/invites", `{"role":"admin"}`, admin)
	if code != http.StatusCreated {
		t.Fatalf("admin invites an admin: %d %s", code, body)
	}

	code, body = doJSONHeaders(t, app, http.MethodPost, "/organizations/org1/invites", `{"role":"admin"}`,
		map[string]string{"X-Plat5-Scopes": "org:members:write"})
	assertInsufficientRole(t, code, body, "You can't assign the admin role.", []string{"org:write", "org:service-accounts:write"})

	code, body = doJSON(t, app, http.MethodPost, "/organizations/org1/invites", `{"role":"owner"}`)
	if code != http.StatusCreated {
		t.Fatalf("unrestricted caller invites an owner: %d %s", code, body)
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

func doJSONHeaders(t *testing.T, app *fiber.App, method, path, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func assertInsufficientRole(t *testing.T, code int, body []byte, message string, missing []string) {
	t.Helper()
	if code != http.StatusForbidden {
		t.Fatalf("want 403, got %d %s", code, body)
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "INSUFFICIENT_SCOPE" || env.Error.Message != message {
		t.Fatalf("got %s", body)
	}
	got := []string{}
	for _, v := range env.Error.Details["scopes"].([]any) {
		got = append(got, v.(string))
	}
	if !reflect.DeepEqual(got, missing) {
		t.Fatalf("details.scopes %v, want %v", got, missing)
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
