package orgs

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestInviteCreateListRevokeAndRedeem(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	h := &Handler{invites: f}
	h.SetInviteAuthorizeURL("https://auth.example.com/authorize?client_id=plat5&response_type=code&redirect_uri=https://console.example.com/callback")
	app := testInviteApp(h, "owner1")

	code, body := doJSON(t, app, http.MethodPost, "/api/organizations/org1/invites", `{"role":"member","email":"a@b.com"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", code, body)
	}
	var created InviteResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Token == "" || !LooksLikeInviteToken(created.Token) {
		t.Fatalf("token once: %+v", created)
	}
	if created.URL == "" || !strings.Contains(created.URL, "invite=") {
		t.Fatalf("url: %s", created.URL)
	}
	if created.Email == nil || *created.Email != "a@b.com" {
		t.Fatalf("email: %+v", created.Email)
	}
	token := created.Token
	inviteID := created.ID

	code, body = doJSON(t, app, http.MethodGet, "/api/organizations/org1/invites", "")
	if code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", code, body)
	}
	var listed ListInvitesResponse
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Invites) != 1 || listed.Invites[0].Token != "" {
		t.Fatalf("list must omit token: %+v", listed)
	}

	inviteeApp := testInviteApp(h, "invitee1")
	code, body = doJSON(t, inviteeApp, http.MethodPost, "/api/invites/redeem", `{"token":"`+token+`"}`)
	if code != http.StatusOK {
		t.Fatalf("redeem status=%d body=%s", code, body)
	}
	var mem MemberResponse
	if err := json.Unmarshal(body, &mem); err != nil {
		t.Fatal(err)
	}
	if mem.Status != string(StatusActive) || mem.UserID == nil || *mem.UserID != "invitee1" {
		t.Fatalf("member: %+v", mem)
	}
	if mem.OrganizationID != "org1" || mem.Role != "member" {
		t.Fatalf("org/role: %+v", mem)
	}

	code, body = doJSON(t, inviteeApp, http.MethodPost, "/api/invites/redeem", `{"token":"`+token+`"}`)
	if code != http.StatusNotFound {
		t.Fatalf("one-shot expected 404, got %d %s", code, body)
	}

	code, _ = doJSON(t, app, http.MethodDelete, "/api/organizations/org1/invites/"+inviteID, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke already-used: %d", code)
	}
}

func TestInviteExpireRevokeAndUnknown(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	h := &Handler{invites: f}
	app := testInviteApp(h, "owner1")

	now := time.Now().UTC()
	expiredTok, err := GenerateInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	expired := &Invite{
		ID:             "inv-expired",
		OrganizationID: "org1",
		Role:           RoleMember,
		TokenHash:      HashInviteToken(expiredTok),
		TokenPrefix:    InviteDisplayPrefix(expiredTok),
		CreatedBy:      "owner1",
		ExpiresAt:      now.Add(-time.Minute),
		CreatedAt:      now.Add(-time.Hour),
	}
	if err := f.CreateInvite(context.Background(), expired); err != nil {
		t.Fatal(err)
	}

	inviteeApp := testInviteApp(h, "u2")
	code, _ := doJSON(t, inviteeApp, http.MethodPost, "/api/invites/redeem", `{"token":"`+expiredTok+`"}`)
	if code != http.StatusNotFound {
		t.Fatalf("expired: %d", code)
	}

	code, body := doJSON(t, app, http.MethodPost, "/api/organizations/org1/invites", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created InviteResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	liveTok := created.Token

	code, _ = doJSON(t, app, http.MethodDelete, "/api/organizations/org1/invites/"+created.ID, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	code, _ = doJSON(t, inviteeApp, http.MethodPost, "/api/invites/redeem", `{"token":"`+liveTok+`"}`)
	if code != http.StatusNotFound {
		t.Fatalf("revoked: %d", code)
	}

	code, body = doJSON(t, inviteeApp, http.MethodPost, "/api/invites/redeem", `{"token":"inv_notarealtoken00000000000000000000000000"}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown: %d %s", code, body)
	}
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["code"] != "NOT_FOUND" {
		t.Fatalf("envelope: %s", body)
	}
	if strings.Contains(string(body), "org1") {
		t.Fatal("404 must not leak org id")
	}
}

func TestInviteRedeemDuplicateMemberIdempotent(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	uid := "already"
	f.members[memberKey("org1", "already")] = &Member{
		ID:             "m-already",
		OrganizationID: "org1",
		UserID:         &uid,
		Role:           RoleMember,
		Status:         StatusActive,
	}
	h := &Handler{invites: f}
	app := testInviteApp(h, "owner1")

	_, body := doJSON(t, app, http.MethodPost, "/api/organizations/org1/invites", `{}`)
	var created InviteResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	inviteeApp := testInviteApp(h, "already")
	code, body := doJSON(t, inviteeApp, http.MethodPost, "/api/invites/redeem", `{"token":"`+created.Token+`"}`)
	if code != http.StatusOK {
		t.Fatalf("idempotent redeem: %d %s", code, body)
	}
	var mem MemberResponse
	if err := json.Unmarshal(body, &mem); err != nil {
		t.Fatal(err)
	}
	if mem.ID != "m-already" || mem.Status != "active" {
		t.Fatalf("existing member: %+v", mem)
	}
}

func TestInviteInternalRedeem(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	h := &Handler{invites: f}
	app := testInviteApp(h, "owner1")

	_, body := doJSON(t, app, http.MethodPost, "/api/organizations/org1/invites", `{"role":"admin"}`)
	var created InviteResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	internal := testInviteApp(h, "")
	code, body := doJSON(t, internal, http.MethodPost, "/internal/invites/redeem",
		`{"token":"`+created.Token+`","user_id":"from-auth"}`)
	if code != http.StatusOK {
		t.Fatalf("internal redeem: %d %s", code, body)
	}
	var mem MemberResponse
	if err := json.Unmarshal(body, &mem); err != nil {
		t.Fatal(err)
	}
	if mem.Role != "admin" || mem.UserID == nil || *mem.UserID != "from-auth" {
		t.Fatalf("%+v", mem)
	}
}

func TestInviteCreateForbiddenForMember(t *testing.T) {
	f := newFakeInvites()
	uid := "mem1"
	f.members[memberKey("org1", "mem1")] = &Member{
		ID:             "m1",
		OrganizationID: "org1",
		UserID:         &uid,
		Role:           RoleMember,
		Status:         StatusActive,
	}
	h := &Handler{invites: f}
	app := testInviteApp(h, "mem1")
	code, _ := doJSON(t, app, http.MethodPost, "/api/organizations/org1/invites", `{}`)
	if code != http.StatusForbidden {
		t.Fatalf("member create invite: %d", code)
	}
}

func TestInviteNoPendingMemberOnCreate(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	h := &Handler{invites: f}
	app := testInviteApp(h, "owner1")
	doJSON(t, app, http.MethodPost, "/api/organizations/org1/invites", `{}`)
	if len(f.members) != 1 {
		t.Fatalf("create invite must not insert a member, got %d", len(f.members))
	}
}
