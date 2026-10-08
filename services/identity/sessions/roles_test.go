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

func starterSet(t *testing.T) *roles.Set {
	t.Helper()
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
	return set
}

func TestSessionMintReturnsRoleAndItsLabels(t *testing.T) {
	userID := "user1"
	org := &fakeOrg{member: &orgs.Member{
		ID:             "mem1",
		OrganizationID: "org1",
		UserID:         &userID,
		Role:           "admin",
		Status:         orgs.StatusActive,
	}}
	store := &fakeSessions{}
	h := &Handler{store: store, orgStore: org, prefix: "plat5-ms-1-", roles: starterSet(t)}
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
	if created.Role != "admin" {
		t.Fatalf("role: %s", body)
	}
	if created.Labels == nil || !reflect.DeepEqual(*created.Labels, []string{"org:write", "org:members:write"}) {
		t.Fatalf("role labels at mint: %s", body)
	}

	store.validated = &Validated{
		Session:        store.created,
		OrganizationID: "org1",
		MemberStatus:   string(orgs.StatusActive),
		MemberRole:     "admin",
	}
	assertValidateLabels(t, app, created.Token, []any{"org:write", "org:members:write"})

	store.validated.MemberRole = "member"
	assertValidateLabels(t, app, created.Token, []any{})
}

func assertValidateLabels(t *testing.T, app *fiber.App, token string, want []any) {
	t.Helper()
	code, body := doSessionJSON(t, app, http.MethodPost, "/internal/member-sessions/validate", `{"token":"`+token+`"}`)
	if code != http.StatusOK {
		t.Fatalf("validate: %d %s", code, body)
	}
	raw := mustObject(t, body)
	if raw["valid"] != true || !reflect.DeepEqual(raw["labels"], want) {
		t.Fatalf("validate labels %#v, want %#v (%s)", raw["labels"], want, body)
	}
}
