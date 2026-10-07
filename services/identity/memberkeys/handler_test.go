package memberkeys

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/orgs"
)

const testPrefix = "plat5-mk-1-"

func TestServiceAccountKeysAreMemberKeys(t *testing.T) {
	keys := &fakeKeys{}
	org := &fakeOrgs{}
	org.add(saFixture("org1", "sa1", "mem-sa", orgs.StatusActive))
	org.add(saFixture("org1", "sa-suspended", "mem-suspended", orgs.StatusSuspended))
	org.add(saFixture("org1", "sa-removed", "mem-removed", orgs.StatusRemoved))
	userID := "user1"
	org.members["mem-user"] = &orgs.Member{
		ID:     "mem-user",
		UserID: &userID,
		Status: orgs.StatusActive,
	}
	h := &Handler{store: keys, orgStore: org, prefix: testPrefix}
	app := testKeyApp(h)

	code, body := doJSON(t, app, http.MethodPost, "/organizations/org1/service-accounts/sa1/api-keys", `{"name":"ci","scopes":["widgets:read"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", code, body)
	}
	var created CreateResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "ci" || !strings.HasPrefix(created.Key, testPrefix) || !strings.HasPrefix(created.KeyPrefix, testPrefix) {
		t.Fatalf("create: %+v", created)
	}
	if created.Scopes == nil || len(*created.Scopes) != 1 || (*created.Scopes)[0] != "widgets:read" {
		t.Fatalf("scopes: %+v", created.Scopes)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["member_id"]; ok {
		t.Fatalf("create must not echo member_id: %s", body)
	}
	if _, ok := raw["key"]; !ok {
		t.Fatalf("plaintext missing: %s", body)
	}
	if len(keys.keys) != 1 || keys.keys[0].MemberID != "mem-sa" {
		t.Fatalf("stored member: %+v", keys.keys)
	}

	code, body = doJSON(t, app, http.MethodGet, "/organizations/org1/service-accounts/sa1/api-keys", "")
	if code != http.StatusOK {
		t.Fatalf("sa list status=%d body=%s", code, body)
	}
	listed := decodeKeys(t, body)
	if len(listed.Keys) != 1 || listed.Keys[0].ID != created.ID || listed.Keys[0].KeyPrefix != created.KeyPrefix {
		t.Fatalf("sa list: %+v", listed)
	}
	if strings.Contains(string(body), created.Key) {
		t.Fatalf("list echoed secret: %s", body)
	}

	code, body = doJSON(t, app, http.MethodGet, "/members/mem-sa/api-keys", "")
	if code != http.StatusOK {
		t.Fatalf("member list status=%d body=%s", code, body)
	}
	if decodeKeys(t, body).Keys[0].ID != created.ID {
		t.Fatalf("member list is a different row: %s", body)
	}

	code, body = doJSON(t, app, http.MethodPost, "/members/mem-sa/api-keys", `{"name":"self"}`)
	if code != http.StatusCreated {
		t.Fatalf("self create status=%d body=%s", code, body)
	}
	var self CreateResponse
	if err := json.Unmarshal(body, &self); err != nil {
		t.Fatal(err)
	}
	code, body = doJSON(t, app, http.MethodGet, "/organizations/org1/service-accounts/sa1/api-keys", "")
	if code != http.StatusOK {
		t.Fatalf("list after self create: %d %s", code, body)
	}
	if len(decodeKeys(t, body).Keys) != 2 {
		t.Fatalf("self-created key missing from sa list: %s", body)
	}

	code, body = doJSON(t, app, http.MethodDelete, "/members/mem-sa/api-keys/"+created.ID, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke via member path: %d %s", code, body)
	}
	code, body = doJSON(t, app, http.MethodGet, "/organizations/org1/service-accounts/sa1/api-keys", "")
	if code != http.StatusOK {
		t.Fatal(code, string(body))
	}
	var revoked *KeyResponse
	for i := range decodeKeys(t, body).Keys {
		k := decodeKeys(t, body).Keys[i]
		if k.ID == created.ID {
			revoked = &k
		}
	}
	if revoked == nil || revoked.RevokedAt == nil {
		t.Fatalf("revoke did not show on sa list: %s", body)
	}

	code, body = doJSON(t, app, http.MethodDelete, "/organizations/org1/service-accounts/sa1/api-keys/"+self.ID, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke via sa path: %d %s", code, body)
	}
	code, _ = doJSON(t, app, http.MethodDelete, "/organizations/org1/service-accounts/sa1/api-keys/"+self.ID, "")
	if code != http.StatusNoContent {
		t.Fatalf("idempotent revoke: %d", code)
	}
}

func TestServiceAccountKeyAddress(t *testing.T) {
	keys := &fakeKeys{}
	org := &fakeOrgs{}
	org.add(saFixture("org1", "sa1", "mem-sa", orgs.StatusActive))
	org.add(saFixture("org1", "sa2", "mem-other", orgs.StatusActive))
	org.add(saFixture("org1", "sa-suspended", "mem-suspended", orgs.StatusSuspended))
	org.add(saFixture("org1", "sa-removed", "mem-removed", orgs.StatusRemoved))
	h := &Handler{store: keys, orgStore: org, prefix: testPrefix}
	app := testKeyApp(h)

	code, body := doJSON(t, app, http.MethodPost, "/organizations/org1/service-accounts/sa1/api-keys", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created CreateResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "Unnamed Key" {
		t.Fatalf("default name: %+v", created)
	}

	code, body = doJSON(t, app, http.MethodDelete, "/organizations/org1/service-accounts/sa2/api-keys/"+created.ID, "")
	assertNotFound(t, code, body, "api_key", created.ID)

	code, body = doJSON(t, app, http.MethodGet, "/organizations/org1/service-accounts/missing/api-keys", "")
	assertNotFound(t, code, body, "service_account", "missing")

	code, body = doJSON(t, app, http.MethodGet, "/organizations/other-org/service-accounts/sa1/api-keys", "")
	assertNotFound(t, code, body, "service_account", "sa1")

	code, body = doJSON(t, app, http.MethodPost, "/organizations/org1/service-accounts/sa-removed/api-keys", `{}`)
	assertNotFound(t, code, body, "service_account", "sa-removed")

	code, body = doJSON(t, app, http.MethodPost, "/organizations/org1/service-accounts/sa-suspended/api-keys", `{"name":"paused"}`)
	if code != http.StatusCreated {
		t.Fatalf("suspended must be addressable: %d %s", code, body)
	}
	if keys.keys[len(keys.keys)-1].MemberID != "mem-suspended" {
		t.Fatalf("suspended member: %+v", keys.keys[len(keys.keys)-1])
	}

	code, body = doJSON(t, app, http.MethodPost, "/organizations/org1/service-accounts/sa1/api-keys", `{"scopes":["BAD"]}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("bad scopes: %d %s", code, body)
	}

	code, body = doJSON(t, app, http.MethodGet, "/organizations/org1/service-accounts/sa2/api-keys", "")
	if code != http.StatusOK {
		t.Fatalf("empty list: %d %s", code, body)
	}
	if len(decodeKeys(t, body).Keys) != 0 {
		t.Fatalf("other sa saw the key: %s", body)
	}

	code, body = doJSON(t, app, http.MethodGet, "/organizations/org1/service-accounts/sa1/api-keys", "")
	if code == http.StatusTeapot {
		t.Fatalf("service-account item route swallowed api-keys")
	}
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
}

func TestMemberKeyMissingParent(t *testing.T) {
	h := &Handler{store: &fakeKeys{}, orgStore: &fakeOrgs{}, prefix: testPrefix}
	app := testKeyApp(h)
	code, body := doJSON(t, app, http.MethodGet, "/members/missing/api-keys", "")
	assertNotFound(t, code, body, "member", "missing")
}

func saFixture(orgID, saID, memberID string, status orgs.Status) *orgs.ServiceAccount {
	return &orgs.ServiceAccount{
		ID:             saID,
		OrganizationID: orgID,
		MemberID:       memberID,
		Name:           saID,
		Status:         status,
		CreatedAt:      time.Unix(0, 0).UTC(),
		UpdatedAt:      time.Unix(0, 0).UTC(),
	}
}

func testKeyApp(h *Handler) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: errors.FiberErrorHandler})
	app.Get("/organizations/:organization_id/service-accounts/:service_account_id", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusTeapot)
	})
	h.MountServiceAccountKeys(app)
	h.MountPublic(app.Group("/members"))
	return app
}

func TestRestrictedCallerCannotMintWiderMemberKey(t *testing.T) {
	keys := &fakeKeys{}
	org := &fakeOrgs{}
	org.add(saFixture("org1", "sa1", "mem-sa", orgs.StatusActive))
	userID := "user1"
	org.members["mem-user"] = &orgs.Member{
		ID:     "mem-user",
		UserID: &userID,
		Status: orgs.StatusActive,
	}
	h := &Handler{store: keys, orgStore: org, prefix: testPrefix}
	app := testKeyApp(h)
	caller := map[string]string{"X-Plat5-Scopes": "projects:read"}

	code, body := doJSONHeader(t, app, http.MethodPost, "/members/mem-user/api-keys", `{}`, caller)
	if code != http.StatusCreated {
		t.Fatalf("inherit status=%d body=%s", code, body)
	}
	created := decodeCreate(t, body)
	if created.Scopes == nil || len(*created.Scopes) != 1 || (*created.Scopes)[0] != "projects:read" {
		t.Fatalf("omitted scopes must inherit, not null: %+v", created.Scopes)
	}
	if keys.keys[0].Scopes == nil || len(keys.keys[0].Scopes) != 1 || keys.keys[0].Scopes[0] != "projects:read" {
		t.Fatalf("stored: %#v", keys.keys[0].Scopes)
	}

	code, body = doJSONHeader(t, app, http.MethodPost, "/members/mem-user/api-keys", `{"scopes":["admin"]}`, caller)
	assertInsufficientScope(t, code, body, "admin")
	if len(keys.keys) != 1 {
		t.Fatalf("rejected mint was stored: %d", len(keys.keys))
	}

	code, body = doJSONHeader(t, app, http.MethodPost, "/members/mem-user/api-keys", `{"scopes":["projects:read","admin"]}`, caller)
	assertInsufficientScope(t, code, body, "admin")

	code, body = doJSONHeader(t, app, http.MethodPost, "/members/mem-user/api-keys", `{"scopes":[]}`, caller)
	if code != http.StatusCreated {
		t.Fatalf("narrower empty: %d %s", code, body)
	}
	narrowed := decodeCreate(t, body)
	if narrowed.Scopes == nil || len(*narrowed.Scopes) != 0 {
		t.Fatalf("explicit empty must stay empty: %+v", narrowed.Scopes)
	}

	code, body = doJSONHeader(t, app, http.MethodPost, "/organizations/org1/service-accounts/sa1/api-keys", `{"scopes":["admin"]}`, caller)
	assertInsufficientScope(t, code, body, "admin")
	code, body = doJSONHeader(t, app, http.MethodPost, "/organizations/org1/service-accounts/sa1/api-keys", `{}`, caller)
	if code != http.StatusCreated {
		t.Fatalf("sa inherit: %d %s", code, body)
	}
	saKey := decodeCreate(t, body)
	if saKey.Scopes == nil || len(*saKey.Scopes) != 1 || (*saKey.Scopes)[0] != "projects:read" {
		t.Fatalf("sa scopes: %+v", saKey.Scopes)
	}
	if keys.keys[len(keys.keys)-1].MemberID != "mem-sa" {
		t.Fatalf("sa member: %+v", keys.keys[len(keys.keys)-1])
	}

	code, body = doJSON(t, app, http.MethodPost, "/members/mem-user/api-keys", `{"scopes":["admin"]}`)
	if code != http.StatusCreated {
		t.Fatalf("unrestricted caller: %d %s", code, body)
	}
	open := decodeCreate(t, body)
	if open.Scopes == nil || len(*open.Scopes) != 1 || (*open.Scopes)[0] != "admin" {
		t.Fatalf("unrestricted may request admin: %+v", open.Scopes)
	}
	code, body = doJSON(t, app, http.MethodPost, "/members/mem-user/api-keys", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("unrestricted omit: %d %s", code, body)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if scopes, ok := raw["scopes"]; !ok || scopes != nil {
		t.Fatalf("unrestricted omit must stay null: %s", body)
	}

	code, body = doJSONHeader(t, app, http.MethodPost, "/members/mem-user/api-keys", `{}`, map[string]string{"X-Plat5-Scopes": "NOT A SCOPE"})
	if code != http.StatusInternalServerError {
		t.Fatalf("bad caller header: %d %s", code, body)
	}
}

func doJSON(t *testing.T, app *fiber.App, method, path, body string) (int, []byte) {
	t.Helper()
	return doJSONHeader(t, app, method, path, body, nil)
}

func doJSONHeader(t *testing.T, app *fiber.App, method, path, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
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

func decodeCreate(t *testing.T, body []byte) CreateResponse {
	t.Helper()
	var created CreateResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	return created
}

func assertInsufficientScope(t *testing.T, code int, body []byte, missing string) {
	t.Helper()
	if code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", code, body)
	}
	var env struct {
		Error struct {
			Type    string         `json:"type"`
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Type != "invalid_request_error" || env.Error.Code != "INSUFFICIENT_SCOPE" {
		t.Fatalf("envelope: %s", body)
	}
	if !strings.Contains(env.Error.Message, missing) {
		t.Fatalf("message %q does not name %s", env.Error.Message, missing)
	}
	list, ok := env.Error.Details["scopes"].([]any)
	if !ok {
		t.Fatalf("details: %s", body)
	}
	found := false
	for _, item := range list {
		if item == missing {
			found = true
		}
	}
	if !found {
		t.Fatalf("details scopes missing %s: %s", missing, body)
	}
}

func decodeKeys(t *testing.T, body []byte) ListResponse {
	t.Helper()
	var listed ListResponse
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	return listed
}

func assertNotFound(t *testing.T, code int, body []byte, resource, id string) {
	t.Helper()
	if code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", code, body)
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "NOT_FOUND" || env.Error.Details["resource"] != resource || env.Error.Details["id"] != id {
		t.Fatalf("not found: %s", body)
	}
}

type fakeKeys struct {
	keys      []*APIKey
	validated *Validated
}

func (f *fakeKeys) Create(_ context.Context, key *APIKey) error {
	f.keys = append(f.keys, key)
	return nil
}

func (f *fakeKeys) GetByHash(context.Context, string) (*Validated, error) {
	if f.validated == nil {
		return nil, ErrNotFound
	}
	return f.validated, nil
}

func (f *fakeKeys) List(_ context.Context, memberID string, limit int, startingAfter string) ([]*APIKey, bool, error) {
	var out []*APIKey
	for _, k := range f.keys {
		if k.MemberID != memberID {
			continue
		}
		if startingAfter != "" && k.ID <= startingAfter {
			continue
		}
		out = append(out, k)
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

func (f *fakeKeys) Revoke(_ context.Context, memberID, keyID string) (*APIKey, error) {
	for _, k := range f.keys {
		if k.ID == keyID && k.MemberID == memberID {
			if k.RevokedAt == nil {
				now := time.Now().UTC()
				k.RevokedAt = &now
			}
			return k, nil
		}
	}
	return nil, ErrNotFound
}

type fakeOrgs struct {
	members map[string]*orgs.Member
	sas     map[string]*orgs.ServiceAccount
}

func (f *fakeOrgs) add(sa *orgs.ServiceAccount) {
	if f.members == nil {
		f.members = map[string]*orgs.Member{}
	}
	if f.sas == nil {
		f.sas = map[string]*orgs.ServiceAccount{}
	}
	f.sas[sa.OrganizationID+"\x00"+sa.ID] = sa
	saID := sa.ID
	f.members[sa.MemberID] = &orgs.Member{
		ID:               sa.MemberID,
		OrganizationID:   sa.OrganizationID,
		ServiceAccountID: &saID,
		Role:             sa.Role,
		Status:           sa.Status,
	}
}

func (f *fakeOrgs) GetMember(_ context.Context, memberID string) (*orgs.Member, error) {
	m, ok := f.members[memberID]
	if !ok {
		return nil, orgs.ErrNotFound
	}
	return m, nil
}

func (f *fakeOrgs) GetServiceAccount(_ context.Context, organizationID, serviceAccountID string) (*orgs.ServiceAccount, error) {
	sa, ok := f.sas[organizationID+"\x00"+serviceAccountID]
	if !ok || sa.Status == orgs.StatusRemoved {
		return nil, orgs.ErrNotFound
	}
	return sa, nil
}
