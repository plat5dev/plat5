package orgs

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	apierrors "github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/middleware"
)

type fakeInvites struct {
	mu      sync.Mutex
	members map[string]*Member // orgID+"/"+userID
	invites map[string]*Invite // id
	byHash  map[string]*Invite
}

func newFakeInvites() *fakeInvites {
	return &fakeInvites{
		members: map[string]*Member{},
		invites: map[string]*Invite{},
		byHash:  map[string]*Invite{},
	}
}

func memberKey(orgID, userID string) string { return orgID + "/" + userID }

func cloneInvite(inv *Invite) *Invite {
	cp := *inv
	if inv.Email != nil {
		e := *inv.Email
		cp.Email = &e
	}
	if inv.Token != nil {
		t := *inv.Token
		cp.Token = &t
	}
	if inv.MaxUses != nil {
		n := *inv.MaxUses
		cp.MaxUses = &n
	}
	return &cp
}

func (f *fakeInvites) GetActiveMemberForUser(_ context.Context, organizationID, userID string) (*Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.members[memberKey(organizationID, userID)]
	if m == nil || m.Status != StatusActive {
		return nil, ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (f *fakeInvites) CreateInvite(_ context.Context, inv *Invite) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := cloneInvite(inv)
	if cp.Status == "" {
		cp.Status = InviteStatusActive
	}
	f.invites[inv.ID] = cp
	f.byHash[inv.TokenHash] = cp
	return nil
}

func (f *fakeInvites) ListInvites(_ context.Context, organizationID string, limit int, startingAfter string) ([]*Invite, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	var all []*Invite
	for _, inv := range f.invites {
		if inv.OrganizationID != organizationID {
			continue
		}
		if inv.Status == InviteStatusActive && !now.Before(inv.ExpiresAt) {
			expireInvite(inv)
		}
		if startingAfter != "" && inv.ID <= startingAfter {
			continue
		}
		all = append(all, cloneInvite(inv))
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	hasMore := len(all) > limit
	if hasMore {
		all = all[:limit]
	}
	return all, hasMore, nil
}

func (f *fakeInvites) RevokeInvite(_ context.Context, organizationID, inviteID string) (*Invite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv := f.invites[inviteID]
	if inv == nil || inv.OrganizationID != organizationID {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	if inv.Status == InviteStatusActive && !now.Before(inv.ExpiresAt) {
		expireInvite(inv)
	} else if inv.Status == InviteStatusActive {
		inv.Status = InviteStatusRevoked
		inv.Token = nil
	}
	return cloneInvite(inv), nil
}

func (f *fakeInvites) RedeemInvite(_ context.Context, tokenHash, userID string) (*Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv := f.byHash[tokenHash]
	if inv == nil {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	if inv.Status == InviteStatusActive && !now.Before(inv.ExpiresAt) {
		expireInvite(inv)
	}
	if inv.Status != InviteStatusActive {
		return nil, &InviteDeadError{Status: inv.Status}
	}
	if !InviteRedeemable(inv, now) {
		return nil, &InviteDeadError{Status: InviteStatusExpired}
	}
	key := memberKey(inv.OrganizationID, userID)
	existing := f.members[key]
	inv.UseCount++
	if inv.MaxUses != nil && inv.UseCount >= *inv.MaxUses {
		inv.Status = InviteStatusRedeemed
		inv.Token = nil
	}
	if existing != nil && existing.Status != StatusRemoved {
		cp := *existing
		return &cp, nil
	}
	uid := userID
	added := inv.CreatedBy
	m := &Member{
		ID:             "mem_" + userID,
		OrganizationID: inv.OrganizationID,
		UserID:         &uid,
		Role:           inv.Role,
		Status:         StatusActive,
		AddedBy:        &added,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	f.members[key] = m
	cp := *m
	return &cp, nil
}

func testInviteApp(h *Handler, userID string) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		if userID != "" {
			c.Locals(middleware.UserIDKey, userID)
		}
		return c.Next()
	})
	app.Post("/api/organizations/:organization_id/invites", h.CreateInvite)
	app.Get("/api/organizations/:organization_id/invites", h.ListInvites)
	app.Delete("/api/organizations/:organization_id/invites/:invite_id", h.RevokeInvite)
	app.Post("/api/invites/redeem", h.RedeemInvite)
	return app
}

func doJSON(t *testing.T, app *fiber.App, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
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

func seedOwner(f *fakeInvites, orgID, userID string) {
	uid := userID
	f.members[memberKey(orgID, userID)] = &Member{
		ID:             "m-owner",
		OrganizationID: orgID,
		UserID:         &uid,
		Role:           RoleOwner,
		Status:         StatusActive,
	}
}

func seedMember(f *fakeInvites, orgID, userID string) {
	uid := userID
	f.members[memberKey(orgID, userID)] = &Member{
		ID:             "m-" + userID,
		OrganizationID: orgID,
		UserID:         &uid,
		Role:           RoleMember,
		Status:         StatusActive,
	}
}
