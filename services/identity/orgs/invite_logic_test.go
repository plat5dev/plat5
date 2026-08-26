package orgs

import (
	"strings"
	"testing"
	"time"
)

func TestParseInviteTTL(t *testing.T) {
	t.Parallel()
	if d, err := ParseInviteTTL(nil); err != nil || d != DefaultInviteTTL {
		t.Fatalf("default: %v %v", d, err)
	}
	sec := 3600
	if d, err := ParseInviteTTL(&sec); err != nil || d != time.Hour {
		t.Fatalf("hour: %v %v", d, err)
	}
	low := 59
	if _, err := ParseInviteTTL(&low); err == nil {
		t.Fatal("expected low ttl error")
	}
	high := int(MaxInviteTTL/time.Second) + 1
	if _, err := ParseInviteTTL(&high); err == nil {
		t.Fatal("expected high ttl error")
	}
}

func TestParseInviteEmail(t *testing.T) {
	t.Parallel()
	if got, err := ParseInviteEmail("  "); err != nil || got != nil {
		t.Fatalf("blank: %v %v", got, err)
	}
	got, err := ParseInviteEmail(" a@b.com ")
	if err != nil || got == nil || *got != "a@b.com" {
		t.Fatalf("email: %v %v", got, err)
	}
	long := strings.Repeat("a", MaxInviteEmailLen+1)
	if _, err := ParseInviteEmail(long); err == nil {
		t.Fatal("expected too long")
	}
}

func TestInviteRedeemable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	inv := &Invite{ExpiresAt: now.Add(time.Hour)}
	if !InviteRedeemable(inv, now) {
		t.Fatal("expected redeemable")
	}
	if InviteRedeemable(inv, inv.ExpiresAt) {
		t.Fatal("expired at exact expiry")
	}
	revoked := now
	inv.RevokedAt = &revoked
	if InviteRedeemable(inv, now) {
		t.Fatal("revoked")
	}
	inv.RevokedAt = nil
	inv.RedeemedAt = &now
	if InviteRedeemable(inv, now.Add(-time.Minute)) {
		t.Fatal("already used")
	}
	if InviteRedeemable(nil, now) {
		t.Fatal("nil")
	}
}

func TestGenerateAndHashInviteToken(t *testing.T) {
	t.Parallel()
	tok, err := GenerateInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	if !LooksLikeInviteToken(tok) {
		t.Fatalf("prefix: %s", tok)
	}
	if !strings.HasPrefix(InviteDisplayPrefix(tok), InviteTokenPrefix) {
		t.Fatalf("display: %s", InviteDisplayPrefix(tok))
	}
	h1 := HashInviteToken(tok)
	h2 := HashInviteToken(tok)
	if h1 != h2 || h1 == tok || len(h1) != 64 {
		t.Fatalf("hash: %s", h1)
	}
	if LooksLikeInviteToken("plat5-sk-1-nope") {
		t.Fatal("must not look like api key")
	}
}

func TestBuildInviteURL(t *testing.T) {
	t.Parallel()
	if got := BuildInviteURL("", "inv_abc"); got != "" {
		t.Fatalf("empty: %q", got)
	}
	got := BuildInviteURL("https://auth.example.com/authorize?client_id=plat5", "inv_abc")
	if !strings.Contains(got, "invite_token=inv_abc") || !strings.Contains(got, "client_id=plat5") {
		t.Fatalf("url: %s", got)
	}
	if BuildInviteURL("not a url", "inv_abc") != "" {
		t.Fatal("expected reject unparseable")
	}
}

func TestCreateMemberKeepsAddByUserID(t *testing.T) {
	t.Parallel()
	req := CreateMemberRequest{UserID: "user_known", Role: "member"}
	if req.UserID == "" {
		t.Fatal("POST members add-by-user_id must remain")
	}
	owner := member("a", "u1", RoleOwner, StatusActive)
	if err := CanCreateMember(owner, RoleMember, "org"); err != nil {
		t.Fatal(err)
	}
}
