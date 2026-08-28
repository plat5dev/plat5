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
		t.Fatalf("token: %+v", created)
	}
	if created.Status != string(InviteStatusActive) {
		t.Fatalf("status: %+v", created)
	}
	if created.MaxUses == nil || *created.MaxUses != 1 {
		t.Fatalf("default max_uses: %+v", created.MaxUses)
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
	if len(listed.Invites) != 1 || listed.Invites[0].Token != token {
		t.Fatalf("owner list must include token: %+v", listed)
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

	code, body = doJSON(t, inviteeApp, http.MethodPost, "/api/invites/redeem", `{"token":"`+token+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("spent expected 409, got %d %s", code, body)
	}
	assertConflictStatus(t, body, "redeemed")

	code, _ = doJSON(t, app, http.MethodDelete, "/api/organizations/org1/invites/"+inviteID, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke already-used: %d", code)
	}
}

func TestInviteListOmitsTokenForMember(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	seedMember(f, "org1", "mem1")
	h := &Handler{invites: f}
	ownerApp := testInviteApp(h, "owner1")
	_, body := doJSON(t, ownerApp, http.MethodPost, "/api/organizations/org1/invites", `{}`)
	var created InviteResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	memApp := testInviteApp(h, "mem1")
	code, body := doJSON(t, memApp, http.MethodGet, "/api/organizations/org1/invites", "")
	if code != http.StatusOK {
		t.Fatalf("member list: %d %s", code, body)
	}
	var listed ListInvitesResponse
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Invites) != 1 {
		t.Fatalf("member should see the row: %+v", listed)
	}
	if listed.Invites[0].Token != "" {
		t.Fatalf("member list must omit token: %+v", listed.Invites[0])
	}
	if listed.Invites[0].Status != string(InviteStatusActive) || listed.Invites[0].TokenPrefix == "" {
		t.Fatalf("member still gets prefix/status: %+v", listed.Invites[0])
	}
}

func TestInviteMaxUsesUnlimitedStaysActive(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	h := &Handler{invites: f}
	app := testInviteApp(h, "owner1")

	code, body := doJSON(t, app, http.MethodPost, "/api/organizations/org1/invites", `{"max_uses":null}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created InviteResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.MaxUses != nil {
		t.Fatalf("unlimited must be null max_uses: %+v", created.MaxUses)
	}
	token := created.Token

	a := testInviteApp(h, "u1")
	code, body = doJSON(t, a, http.MethodPost, "/api/invites/redeem", `{"token":"`+token+`"}`)
	if code != http.StatusOK {
		t.Fatalf("first redeem: %d %s", code, body)
	}
	code, body = doJSON(t, app, http.MethodGet, "/api/organizations/org1/invites", "")
	if code != http.StatusOK {
		t.Fatal(code)
	}
	var listed ListInvitesResponse
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Invites[0].Token != token || listed.Invites[0].Status != string(InviteStatusActive) || listed.Invites[0].UseCount != 1 {
		t.Fatalf("still active with token: %+v", listed.Invites[0])
	}

	b := testInviteApp(h, "u2")
	code, body = doJSON(t, b, http.MethodPost, "/api/invites/redeem", `{"token":"`+token+`"}`)
	if code != http.StatusOK {
		t.Fatalf("second redeem: %d %s", code, body)
	}
}

func TestInviteMaxUsesZeroRejected(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	h := &Handler{invites: f}
	app := testInviteApp(h, "owner1")
	for _, body := range []string{`{"max_uses":0}`, `{"max_uses":-1}`} {
		code, resp := doJSON(t, app, http.MethodPost, "/api/organizations/org1/invites", body)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s", body, code, resp)
		}
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
		Token:          &expiredTok,
		TokenHash:      HashInviteToken(expiredTok),
		TokenPrefix:    InviteDisplayPrefix(expiredTok),
		Status:         InviteStatusActive,
		CreatedBy:      "owner1",
		ExpiresAt:      now.Add(-time.Minute),
		CreatedAt:      now.Add(-time.Hour),
	}
	if err := f.CreateInvite(context.Background(), expired); err != nil {
		t.Fatal(err)
	}

	inviteeApp := testInviteApp(h, "u2")
	code, body := doJSON(t, inviteeApp, http.MethodPost, "/api/invites/redeem", `{"token":"`+expiredTok+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("expired: %d %s", code, body)
	}
	assertConflictStatus(t, body, "expired")

	code, body = doJSON(t, app, http.MethodPost, "/api/organizations/org1/invites", `{}`)
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
	code, body = doJSON(t, inviteeApp, http.MethodPost, "/api/invites/redeem", `{"token":"`+liveTok+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("revoked: %d %s", code, body)
	}
	assertConflictStatus(t, body, "revoked")

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
	stored := f.invites[created.ID]
	if stored.UseCount != 1 || stored.Status != InviteStatusRedeemed {
		t.Fatalf("already-member still consumes a use: %+v", stored)
	}
	code, body = doJSON(t, inviteeApp, http.MethodPost, "/api/invites/redeem", `{"token":"`+created.Token+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("second redeem after spent: %d %s", code, body)
	}
	if stored.UseCount != 1 {
		t.Fatalf("do not double-increment: %d", stored.UseCount)
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

func assertConflictStatus(t *testing.T, body []byte, status string) {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["code"] != "CONFLICT" {
		t.Fatalf("code: %s", body)
	}
	details, _ := errObj["details"].(map[string]any)
	if details["status"] != status {
		t.Fatalf("status want %s: %s", status, body)
	}
}
