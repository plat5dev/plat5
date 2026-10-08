package sessions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/orgs"
)

func TestSessionForWildcardRoleCarriesEveryLabel(t *testing.T) {
	userID := "user1"
	org := &fakeOrg{member: &orgs.Member{
		ID:             "mem1",
		OrganizationID: "org1",
		UserID:         &userID,
		Role:           "owner",
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
	if created.MemberID != "mem1" || created.OrganizationID != "org1" || !strings.HasPrefix(created.Token, "plat5-ms-1-") {
		t.Fatalf("create: %+v", created)
	}
	if labels, ok := mustObject(t, body)["labels"]; !ok || labels != nil {
		t.Fatalf(`["*"]: labels must be null: %s`, body)
	}
	if _, ok := mustObject(t, body)["user_id"]; ok {
		t.Fatalf("mint must not return user_id: %s", body)
	}

	store.validated = &Validated{
		Session:        store.created,
		OrganizationID: "org1",
		MemberStatus:   string(orgs.StatusActive),
		MemberRole:     "owner",
	}
	code, body = doSessionJSON(t, app, http.MethodPost, "/internal/member-sessions/validate", `{"token":"`+created.Token+`"}`)
	if code != http.StatusOK {
		t.Fatalf("validate: %d %s", code, body)
	}
	payload := mustObject(t, body)
	if _, ok := payload["user_id"]; ok {
		t.Fatalf("validate user_id: %s", body)
	}
	if labels, ok := payload["labels"]; !ok || labels != nil {
		t.Fatalf("validate labels must be null: %s", body)
	}
}

func TestSessionMintStill404WhenNotAMember(t *testing.T) {
	h := &Handler{store: &fakeSessions{}, orgStore: &fakeOrg{err: orgs.ErrNotFound}, prefix: "plat5-ms-1-", roles: starterSet(t)}
	app := fiber.New(fiber.Config{ErrorHandler: errors.FiberErrorHandler})
	h.MountPublic(app.Group("/users"))
	code, body := doSession(t, app, http.MethodPost, "/users/user1/organizations/org1/session")
	if code != http.StatusNotFound {
		t.Fatalf("not a member: %d %s", code, body)
	}
}

type fakeSessions struct {
	created   *Session
	validated *Validated
}

func (f *fakeSessions) Create(_ context.Context, session *Session) error {
	cp := *session
	f.created = &cp
	return nil
}

func (f *fakeSessions) GetByHash(context.Context, string) (*Validated, error) {
	if f.validated == nil {
		return nil, ErrNotFound
	}
	return f.validated, nil
}

type fakeOrg struct {
	member *orgs.Member
	err    error
}

func (f *fakeOrg) ResolveMember(context.Context, string, string) (*orgs.Member, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.member, nil
}

func doSession(t *testing.T, app *fiber.App, method, path string) (int, []byte) {
	t.Helper()
	return doSessionJSON(t, app, method, path, "")
}

func doSessionJSON(t *testing.T, app *fiber.App, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
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

func mustObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
