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
	app := testInviteApp(h)

	code, body := doJSON(t, app, http.MethodPost, "/organizations/org1/invites", `{"email":"a@b.com","created_by":"owner1"}`)
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
	if created.CreatedBy == nil || *created.CreatedBy != "owner1" {
		t.Fatalf("created_by: %+v", created.CreatedBy)
	}
	token := created.Token
	inviteID := created.ID

	code, body = doJSON(t, app, http.MethodGet, "/organizations/org1/invites", "")
	if code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", code, body)
	}
	var listed ListInvitesResponse
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Invites) != 1 || listed.Invites[0].Token != token {
		t.Fatalf("list must include token: %+v", listed)
	}

	code, body = doJSON(t, app, http.MethodPost, "/users/invitee1/invites/redeem", `{"token":"`+token+`"}`)
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
	if mem.AddedBy == nil || *mem.AddedBy != "owner1" {
		t.Fatalf("added_by: %+v", mem.AddedBy)
	}

	code, body = doJSON(t, app, http.MethodGet, "/organizations/org1/invites", "")
	if code != http.StatusOK {
		t.Fatalf("list after redeem: %d %s", code, body)
	}
	listed = ListInvitesResponse{}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Invites[0].Token != "" || listed.Invites[0].Status != string(InviteStatusRedeemed) || listed.Invites[0].UseCount != 1 {
		t.Fatalf("spent row: %+v", listed.Invites[0])
	}

	code, body = doJSON(t, app, http.MethodPost, "/users/invitee1/invites/redeem", `{"token":"`+token+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("spent expected 409, got %d %s", code, body)
	}
	assertConflictStatus(t, body, "redeemed")

	code, body = doJSON(t, app, http.MethodDelete, "/organizations/org1/invites/"+inviteID, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke already-used: %d %s id=%q", code, body, inviteID)
	}
}

func TestInviteListIncludesTokenWhileActive(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	h := &Handler{invites: f}
	app := testInviteApp(h)
	_, body := doJSON(t, app, http.MethodPost, "/organizations/org1/invites", `{}`)
	var created InviteResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	code, body := doJSON(t, app, http.MethodGet, "/organizations/org1/invites", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	var listed ListInvitesResponse
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Invites) != 1 || listed.Invites[0].Token == "" || listed.Invites[0].Token != created.Token {
		t.Fatalf("active list must include token: %+v", listed)
	}
}

func TestInviteMaxUsesUnlimitedStaysActive(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	h := &Handler{invites: f}
	app := testInviteApp(h)

	code, body := doJSON(t, app, http.MethodPost, "/organizations/org1/invites", `{"max_uses":null}`)
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

	code, body = doJSON(t, app, http.MethodPost, "/users/u1/invites/redeem", `{"token":"`+token+`"}`)
	if code != http.StatusOK {
		t.Fatalf("first redeem: %d %s", code, body)
	}
	code, body = doJSON(t, app, http.MethodGet, "/organizations/org1/invites", "")
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

	code, body = doJSON(t, app, http.MethodPost, "/users/u2/invites/redeem", `{"token":"`+token+`"}`)
	if code != http.StatusOK {
		t.Fatalf("second redeem: %d %s", code, body)
	}
}

func TestInviteMaxUsesZeroRejected(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	h := &Handler{invites: f}
	app := testInviteApp(h)
	for _, payload := range []string{`{"max_uses":0}`, `{"max_uses":-1}`} {
		code, resp := doJSON(t, app, http.MethodPost, "/organizations/org1/invites", payload)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s", payload, code, resp)
		}
	}
}

func TestInviteExpireRevokeAndUnknown(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	h := &Handler{invites: f}
	app := testInviteApp(h)

	now := time.Now().UTC()
	expiredTok, err := GenerateInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	expired := &Invite{
		ID:             "inv-expired",
		OrganizationID: "org1",
		Token:          &expiredTok,
		TokenHash:      HashInviteToken(expiredTok),
		TokenPrefix:    InviteDisplayPrefix(expiredTok),
		Status:         InviteStatusActive,
		CreatedBy:      strPtr("owner1"),
		ExpiresAt:      now.Add(-time.Minute),
		CreatedAt:      now.Add(-time.Hour),
	}
	if err := f.CreateInvite(context.Background(), expired); err != nil {
		t.Fatal(err)
	}

	code, body := doJSON(t, app, http.MethodPost, "/users/u2/invites/redeem", `{"token":"`+expiredTok+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("expired: %d %s", code, body)
	}
	assertConflictStatus(t, body, "expired")

	code, body = doJSON(t, app, http.MethodPost, "/organizations/org1/invites", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created InviteResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	liveTok := created.Token

	code, _ = doJSON(t, app, http.MethodDelete, "/organizations/org1/invites/"+created.ID, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	code, body = doJSON(t, app, http.MethodPost, "/users/u2/invites/redeem", `{"token":"`+liveTok+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("revoked: %d %s", code, body)
	}
	assertConflictStatus(t, body, "revoked")

	code, body = doJSON(t, app, http.MethodPost, "/users/u2/invites/redeem", `{"token":"inv_notarealtoken00000000000000000000000000"}`)
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
		Status:         StatusActive,
	}
	h := &Handler{invites: f}
	app := testInviteApp(h)

	_, body := doJSON(t, app, http.MethodPost, "/organizations/org1/invites", `{}`)
	var created InviteResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}

	code, body := doJSON(t, app, http.MethodPost, "/users/already/invites/redeem", `{"token":"`+created.Token+`"}`)
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

func TestInviteCreateDoesNotCheckCaller(t *testing.T) {
	f := newFakeInvites()
	uid := "mem1"
	f.members[memberKey("org1", "mem1")] = &Member{
		ID:             "m1",
		OrganizationID: "org1",
		UserID:         &uid,
		Status:         StatusActive,
	}
	h := &Handler{invites: f}
	app := testInviteApp(h)
	code, body := doJSON(t, app, http.MethodPost, "/organizations/org1/invites", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("create invite: %d %s", code, body)
	}
}

func TestInviteNoPendingMemberOnCreate(t *testing.T) {
	f := newFakeInvites()
	seedOwner(f, "org1", "owner1")
	h := &Handler{invites: f}
	app := testInviteApp(h)
	doJSON(t, app, http.MethodPost, "/organizations/org1/invites", `{}`)
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
	if details["field"] != "status" || details["value"] != status {
		t.Fatalf("details want field=status value=%s: %s", status, body)
	}
}
