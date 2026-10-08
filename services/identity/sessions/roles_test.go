package sessions

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/orgs"
	"github.com/plat5dev/plat5/identity/roles"
)

func TestSessionMintReturnsRoleAndItsLabels(t *testing.T) {
	set, err := roles.Parse([]byte(`
roles:
  owner: ["*"]
  admin: [org:write, org:members:write]
  member: []
creator_role: owner
default_role: member
`))
	if err != nil {
		t.Fatal(err)
	}
	userID := "user1"
	admin := "admin"
	org := &fakeOrg{member: &orgs.Member{
		ID:             "mem1",
		OrganizationID: "org1",
		UserID:         &userID,
		Role:           &admin,
		Status:         orgs.StatusActive,
	}}
	store := &fakeSessions{}
	h := &Handler{store: store, orgStore: org, prefix: "plat5-ms-1-", roles: set}
	app := fiber.New(fiber.Config{ErrorHandler: errors.FiberErrorHandler})
	h.MountPublic(app.Group("/users"))
	h.MountInternal(app.Group("/internal"))

	code, body := doSession(t, app, http.MethodPost, "/users/user1/organizations/org1/session")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created CreateResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Role == nil || *created.Role != "admin" {
		t.Fatalf("role: %s", body)
	}
	if created.Scopes == nil || !reflect.DeepEqual(*created.Scopes, []string{"org:write", "org:members:write"}) {
		t.Fatalf("role labels at mint: %s", body)
	}

	store.validated = &Validated{
		Session:        store.created,
		OrganizationID: "org1",
		MemberStatus:   string(orgs.StatusActive),
		MemberRole:     &admin,
	}
	assertValidateScopes(t, app, created.Token, []any{"org:write", "org:members:write"})

	member := "member"
	store.validated.MemberRole = &member
	assertValidateScopes(t, app, created.Token, []any{})
}

func assertValidateScopes(t *testing.T, app *fiber.App, token string, want []any) {
	t.Helper()
	code, body := doSessionJSON(t, app, http.MethodPost, "/internal/member-sessions/validate", `{"token":"`+token+`"}`)
	if code != http.StatusOK {
		t.Fatalf("validate: %d %s", code, body)
	}
	raw := mustObject(t, body)
	if raw["valid"] != true || !reflect.DeepEqual(raw["scopes"], want) {
		t.Fatalf("validate scopes %#v, want %#v (%s)", raw["scopes"], want, body)
	}
}
