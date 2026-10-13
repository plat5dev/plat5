package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/directory"
	apierrors "github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/apikey"
	"github.com/plat5dev/plat5/identity/orgs"
	"github.com/plat5dev/plat5/identity/roles"
)

func TestServiceAccountKeysAreMemberKeys(t *testing.T) {
	f := newFixture(t)
	f.addServiceAccount("org1", "sa1", "mem-sa", orgs.StatusActive)
	f.addServiceAccount("org1", "sa-suspended", "mem-suspended", orgs.StatusSuspended)
	f.addServiceAccount("org1", "sa-removed", "mem-removed", orgs.StatusRemoved)

	code, body, hdr := f.do(http.MethodPost, "/organizations/org1/service-accounts/sa1/api-keys", `{"name":"ci"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created CreateKeyResponse
	mustDecode(t, body, &created)
	if created.Name != "ci" || !strings.HasPrefix(created.Key, "plat5-k-2-") || created.KeyPrefix != created.Key[:len("plat5-k-2-")+4] {
		t.Fatalf("create: %+v", created)
	}
	for _, field := range []string{`"member_id"`, `"labels"`, `"user_id"`} {
		if strings.Contains(string(body), field) {
			t.Fatalf("create must not echo %s: %s", field, body)
		}
	}
	if hdr.Get("X-Plat5-Audit-Created") != created.ID {
		t.Fatalf("created header %q, key %q", hdr.Get("X-Plat5-Audit-Created"), created.ID)
	}
	details := hdr.Get("X-Plat5-Audit-Details")
	if strings.Contains(details, created.Key) || !strings.Contains(details, created.KeyPrefix) {
		t.Fatalf("details must carry the display prefix, never the key: %s", details)
	}

	for _, path := range []string{"/organizations/org1/service-accounts/sa1/api-keys", "/members/mem-sa/api-keys"} {
		code, body, _ = f.do(http.MethodGet, path, "")
		var listed ListKeysResponse
		mustDecode(t, body, &listed)
		if code != http.StatusOK || len(listed.Keys) != 1 || listed.Keys[0].ID != created.ID {
			t.Fatalf("list %s: %d %s", path, code, body)
		}
		if strings.Contains(string(body), created.Key) {
			t.Fatalf("list echoed the secret: %s", body)
		}
	}

	code, body, _ = f.do(http.MethodDelete, "/members/mem-sa/api-keys/"+created.ID, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", code, body)
	}
	code, _, _ = f.do(http.MethodDelete, "/organizations/org1/service-accounts/sa1/api-keys/"+created.ID, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke again at the org address: %d", code)
	}

	code, _, _ = f.do(http.MethodPost, "/organizations/org1/service-accounts/sa-suspended/api-keys", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("suspended service account is addressable: %d", code)
	}
	for _, path := range []string{
		"/organizations/org1/service-accounts/sa-removed/api-keys",
		"/organizations/org2/service-accounts/sa1/api-keys",
		"/members/mem-removed/api-keys",
		"/members/nope/api-keys",
	} {
		code, body, _ = f.do(http.MethodGet, path, "")
		if code != http.StatusNotFound {
			t.Fatalf("%s: want 404, got %d %s", path, code, body)
		}
	}
}

func TestKeyUnderAnotherMembershipIsNotFound(t *testing.T) {
	f := newFixture(t)
	f.addUser("alice", directory.StatusActive)
	f.addUser("bob", directory.StatusActive)
	code, body, _ := f.do(http.MethodPost, "/users/alice/api-keys", `{"name":"laptop"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created CreateKeyResponse
	mustDecode(t, body, &created)

	code, _, _ = f.do(http.MethodDelete, "/users/bob/api-keys/"+created.ID, "")
	if code != http.StatusNotFound {
		t.Fatalf("revoke under another user: %d", code)
	}
	code, _, _ = f.do(http.MethodPost, "/users/nobody/api-keys", `{}`)
	if code != http.StatusNotFound {
		t.Fatalf("key for a person not in the directory: %d", code)
	}
}

func TestValidate(t *testing.T) {
	f := newFixture(t)
	f.addUser("alice", directory.StatusActive)
	f.addUser("sam", directory.StatusSuspended)
	f.addMember("org1", "mem-owner", "alice", orgs.StatusActive, rolePtr("owner"))
	f.addMember("org1", "mem-member", "alice", orgs.StatusActive, rolePtr("member"))
	f.addMember("org1", "mem-suspended", "alice", orgs.StatusSuspended, rolePtr("owner"))
	f.addMember("org1", "mem-sam", "sam", orgs.StatusActive, rolePtr("owner"))

	userKey := f.mint(t, "alice", apikey.KindKey, nil)
	ownerKey := f.mint(t, "mem-owner", apikey.KindKey, nil)
	memberKey := f.mint(t, "mem-member", apikey.KindKey, nil)
	session := f.mint(t, "mem-owner", apikey.KindSession, nil)
	expired := f.mint(t, "mem-owner", apikey.KindSession, func(c *Credential) {
		past := f.now.Add(-time.Second)
		c.ExpiresAt = &past
	})
	revoked := f.mint(t, "mem-owner", apikey.KindKey, func(c *Credential) {
		c.RevokedAt = &f.now
	})
	suspendedMember := f.mint(t, "mem-suspended", apikey.KindKey, nil)
	suspendedPerson := f.mint(t, "mem-sam", apikey.KindKey, nil)
	suspendedPersonKey := f.mint(t, "sam", apikey.KindKey, nil)
	v1 := "plat5-mk-1-" + strings.Repeat("A", 43)
	f.store.put(&Credential{ID: "v1", MembershipID: "mem-owner", Kind: apikey.KindKey, KeyHash: apikey.Hash(v1)}, f.found("mem-owner"))

	cases := []struct {
		name string
		body string
		want map[string]any
	}{
		{"user key", cred(userKey), map[string]any{"valid": true, "realm": "directory", "kind": "key", "user_id": "alice"}},
		{"owner key", cred(ownerKey), map[string]any{"valid": true, "realm": "organization", "kind": "key", "member_id": "mem-owner", "organization_id": "org1", "labels": []any{"*"}}},
		{"member key has no labels", cred(memberKey), map[string]any{"valid": true, "realm": "organization", "kind": "key", "member_id": "mem-member", "organization_id": "org1", "labels": []any{}}},
		{"session", cred(session), map[string]any{"valid": true, "realm": "organization", "kind": "session", "member_id": "mem-owner", "organization_id": "org1", "labels": []any{"*"}}},
		{"version 1", cred(v1), map[string]any{"valid": true, "realm": "organization", "kind": "key", "member_id": "mem-owner", "organization_id": "org1", "labels": []any{"*"}}},
		{"expired session", cred(expired), invalid},
		{"revoked key", cred(revoked), invalid},
		{"suspended member", cred(suspendedMember), invalid},
		{"suspended person's member key", cred(suspendedPerson), invalid},
		{"suspended person's user key", cred(suspendedPersonKey), invalid},
		{"other brand", cred(strings.Replace(ownerKey, "plat5-", "acme-", 1)), invalid},
		{"session secret under a key prefix", cred(strings.Replace(session, "-s-2-", "-k-2-", 1)), invalid},
		{"unknown", cred("plat5-k-2-" + strings.Repeat("B", 43)), invalid},
		{"junk", cred("nope"), invalid},
		{"idp", `{"idp":{"issuer":"https://auth.test","subject":"alice-sub"}}`, map[string]any{"valid": true, "realm": "directory", "kind": "idp", "user_id": "alice"}},
		{"idp of a suspended person", `{"idp":{"issuer":"https://auth.test","subject":"sam-sub"}}`, invalid},
		{"idp without subject", `{"idp":{"issuer":"https://auth.test","subject":""}}`, invalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body, _ := f.do(http.MethodPost, "/internal/credentials/validate", tc.body)
			if code != http.StatusOK {
				t.Fatalf("status %d %s", code, body)
			}
			var got map[string]any
			mustDecode(t, body, &got)
			if !equalJSON(got, tc.want) {
				t.Fatalf("got %s, want %v", body, tc.want)
			}
		})
	}

	for _, body := range []string{`{}`, `{"credential":"x","idp":{"issuer":"a","subject":"b"}}`} {
		code, resp, _ := f.do(http.MethodPost, "/internal/credentials/validate", body)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: want 422, got %d %s", body, code, resp)
		}
	}
}

func TestSessionMint(t *testing.T) {
	f := newFixture(t)
	f.addUser("alice", directory.StatusActive)
	f.addMember("org1", "mem-admin", "alice", orgs.StatusActive, rolePtr("admin"))
	f.addMember("org2", "mem-suspended", "alice", orgs.StatusSuspended, rolePtr("admin"))

	code, body, hdr := f.do(http.MethodPost, "/users/alice/organizations/org1/session", "")
	if code != http.StatusCreated {
		t.Fatalf("mint: %d %s", code, body)
	}
	var minted CreateSessionResponse
	mustDecode(t, body, &minted)
	if !strings.HasPrefix(minted.Token, "plat5-s-2-") || minted.MemberID != "mem-admin" || minted.Role == nil || *minted.Role != "admin" {
		t.Fatalf("mint: %s", body)
	}
	if strings.Join(minted.Labels, ",") != "org:write,org:members:write" {
		t.Fatalf("labels: %v", minted.Labels)
	}
	if strings.Contains(string(body), `"user_id"`) {
		t.Fatalf("mint must not echo user_id: %s", body)
	}
	if hdr.Get("X-Plat5-Audit-Organization") != "org1" || hdr.Get("X-Plat5-Audit-Member") != "mem-admin" {
		t.Fatalf("audit org headers: %v", hdr)
	}

	code, body, _ = f.do(http.MethodPost, "/internal/credentials/validate", cred(minted.Token))
	var got map[string]any
	mustDecode(t, body, &got)
	if code != http.StatusOK || got["valid"] != true || got["kind"] != "session" {
		t.Fatalf("validate minted: %d %s", code, body)
	}

	for _, path := range []string{"/users/alice/organizations/org2/session", "/users/alice/organizations/org3/session"} {
		code, body, _ = f.do(http.MethodPost, path, "")
		if code != http.StatusNotFound {
			t.Fatalf("%s: want 404, got %d %s", path, code, body)
		}
	}
}

func TestRolesOffCarryEveryLabel(t *testing.T) {
	f := newFixture(t)
	f.h.roles = nil
	f.addUser("alice", directory.StatusActive)
	f.addMember("org1", "mem", "alice", orgs.StatusActive, nil)
	key := f.mint(t, "mem", apikey.KindKey, nil)
	_, body, _ := f.do(http.MethodPost, "/internal/credentials/validate", cred(key))
	var got map[string]any
	mustDecode(t, body, &got)
	if !equalJSON(got["labels"], []any{"*"}) {
		t.Fatalf("roles off: %s", body)
	}
}

var invalid = map[string]any{"valid": false}

func cred(s string) string {
	b, _ := json.Marshal(map[string]string{"credential": s})
	return string(b)
}

func rolePtr(s string) *string { return &s }

type fixture struct {
	t     *testing.T
	h     *Handler
	app   *fiber.App
	store *fakeStore
	orgs  *fakeOrgs
	dir   *fakePeople
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
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
	f := &fixture{
		t:     t,
		store: &fakeStore{byHash: map[string]*Found{}},
		orgs:  &fakeOrgs{members: map[string]*orgs.Member{}, sas: map[string]*orgs.ServiceAccount{}},
		dir:   &fakePeople{users: map[string]*directory.User{}, links: map[string]string{}},
		now:   time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC),
	}
	f.h = &Handler{store: f.store, orgs: f.orgs, people: f.dir, brand: "plat5", roles: set, now: func() time.Time { return f.now }}
	f.store.lookupMembership = f.found
	app := fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	f.h.MountUser(app.Group("/users"))
	f.h.MountMembers(app.Group("/members"))
	f.h.MountServiceAccountKeys(app)
	f.h.MountInternal(app.Group("/internal"))
	f.app = app
	return f
}

func (f *fixture) addUser(id, status string) {
	f.dir.users[id] = &directory.User{ID: id, Status: status}
	f.dir.links["https://auth.test|"+id+"-sub"] = id
}

func (f *fixture) addMember(orgID, memberID, userID string, status orgs.Status, role *string) {
	uid := userID
	f.orgs.members[memberID] = &orgs.Member{
		ID: memberID, OrganizationID: orgID, Kind: orgs.PrincipalUser, UserID: &uid, Status: status, Role: role,
	}
}

func (f *fixture) addServiceAccount(orgID, saID, memberID string, status orgs.Status) {
	id := saID
	f.orgs.members[memberID] = &orgs.Member{
		ID: memberID, OrganizationID: orgID, Kind: orgs.PrincipalServiceAccount, ServiceAccountID: &id, Status: status,
	}
	f.orgs.sas[saID] = &orgs.ServiceAccount{ID: saID, OrganizationID: orgID, MemberID: memberID, Status: status}
}

// found is what Lookup joins for a membership: the directory when it is a
// person's id, else an org member.
func (f *fixture) found(membershipID string) *Found {
	if u, ok := f.dir.users[membershipID]; ok {
		return &Found{IdentityID: u.ID, PersonStatus: u.Status}
	}
	m := f.orgs.members[membershipID]
	if m == nil {
		return nil
	}
	status := string(m.Status)
	personStatus := directory.StatusActive
	identity := *m.IdentityID()
	if u, ok := f.dir.users[identity]; ok {
		personStatus = u.Status
	}
	org := m.OrganizationID
	return &Found{OrganizationID: &org, IdentityID: identity, MemberStatus: &status, MemberRole: m.Role, PersonStatus: personStatus}
}

// mint stores a credential of kind for membershipID and returns its plaintext.
func (f *fixture) mint(t *testing.T, membershipID string, kind apikey.Kind, edit func(*Credential)) string {
	t.Helper()
	prefix := apikey.Prefix("plat5", kind)
	plaintext, err := apikey.Generate(prefix)
	if err != nil {
		t.Fatal(err)
	}
	c := NewKey(membershipID, "k", plaintext, prefix)
	if kind == apikey.KindSession {
		c = NewSession(membershipID, plaintext, prefix, f.now)
	}
	if edit != nil {
		edit(c)
	}
	f.store.put(c, f.found(membershipID))
	return plaintext
}

func (f *fixture) do(method, path, body string) (int, []byte, http.Header) {
	f.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.app.Test(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp.StatusCode, b, resp.Header
}

func mustDecode(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

type fakeStore struct {
	byHash           map[string]*Found
	lookupMembership func(string) *Found
}

func (s *fakeStore) put(c *Credential, f *Found) {
	cp := *f
	cp.Credential = c
	s.byHash[c.KeyHash] = &cp
}

func (s *fakeStore) Create(_ context.Context, c *Credential) error {
	f := s.lookupMembership(c.MembershipID)
	if f == nil {
		return ErrNoMembership
	}
	s.put(c, f)
	return nil
}

func (s *fakeStore) Lookup(_ context.Context, hash string) (*Found, error) {
	if f, ok := s.byHash[hash]; ok {
		return f, nil
	}
	return nil, ErrNotFound
}

func (s *fakeStore) ListKeys(_ context.Context, membershipID string, _ int, _ string) ([]*Credential, bool, error) {
	var out []*Credential
	for _, f := range s.byHash {
		if f.Credential.MembershipID == membershipID && f.Credential.Kind == apikey.KindKey {
			out = append(out, f.Credential)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, false, nil
}

func (s *fakeStore) RevokeKey(_ context.Context, membershipID, keyID string) (*Credential, error) {
	for _, f := range s.byHash {
		c := f.Credential
		if c.ID == keyID && c.MembershipID == membershipID && c.Kind == apikey.KindKey {
			if c.RevokedAt == nil {
				now := time.Now()
				c.RevokedAt = &now
			}
			return c, nil
		}
	}
	return nil, ErrNotFound
}

type fakeOrgs struct {
	members map[string]*orgs.Member
	sas     map[string]*orgs.ServiceAccount
}

func (f *fakeOrgs) GetMember(_ context.Context, memberID string) (*orgs.Member, error) {
	if m, ok := f.members[memberID]; ok {
		return m, nil
	}
	return nil, orgs.ErrNotFound
}

func (f *fakeOrgs) GetServiceAccount(_ context.Context, orgID, saID string) (*orgs.ServiceAccount, error) {
	sa, ok := f.sas[saID]
	if !ok || sa.OrganizationID != orgID || sa.Status == orgs.StatusRemoved {
		return nil, orgs.ErrNotFound
	}
	return sa, nil
}

func (f *fakeOrgs) ResolveMember(_ context.Context, userID, orgID string) (*orgs.Member, error) {
	for _, m := range f.members {
		if m.OrganizationID == orgID && m.UserID != nil && *m.UserID == userID && m.Status != orgs.StatusRemoved {
			return m, nil
		}
	}
	return nil, orgs.ErrNotFound
}

type fakePeople struct {
	users map[string]*directory.User
	links map[string]string
}

func (f *fakePeople) Get(_ context.Context, userID string) (*directory.User, error) {
	if u, ok := f.users[userID]; ok {
		return u, nil
	}
	return nil, directory.ErrNotFound
}

func (f *fakePeople) Resolve(_ context.Context, issuer, subject string) (string, string, error) {
	id, ok := f.links[issuer+"|"+subject]
	if !ok {
		return "", "", directory.ErrNotFound
	}
	return id, f.users[id].Status, nil
}
