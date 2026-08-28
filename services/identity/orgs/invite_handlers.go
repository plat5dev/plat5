package orgs

import (
	"context"
	stderrors "errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/middleware"
)

type inviteStore interface {
	GetActiveMemberForUser(ctx context.Context, organizationID, userID string) (*Member, error)
	CreateInvite(ctx context.Context, inv *Invite) error
	ListInvites(ctx context.Context, organizationID string, limit int, startingAfter string) ([]*Invite, *string, error)
	RevokeInvite(ctx context.Context, organizationID, inviteID string) (*Invite, error)
	RedeemInvite(ctx context.Context, tokenHash, userID string) (*Member, error)
}

func (h *Handler) inviteStore() inviteStore {
	if h.invites != nil {
		return h.invites
	}
	return h.store
}

type CreateInviteRequest struct {
	Role             string `json:"role"`
	Email            string `json:"email"`
	ExpiresInSeconds *int   `json:"expires_in_seconds"`
}

type InviteResponse struct {
	ID             string  `json:"id"`
	OrganizationID string  `json:"organization_id"`
	Role           string  `json:"role"`
	Email          *string `json:"email"`
	TokenPrefix    string  `json:"token_prefix"`
	Token          string  `json:"token,omitempty"`
	Status         string  `json:"status"`
	MaxUses        *int    `json:"max_uses"`
	UseCount       int     `json:"use_count"`
	ExpiresAt      string  `json:"expires_at"`
	CreatedBy      string  `json:"created_by"`
	CreatedAt      string  `json:"created_at"`
}

type ListInvitesResponse struct {
	Invites []InviteResponse `json:"invites"`
	Last    *string          `json:"last"`
}

type RedeemInviteRequest struct {
	Token string `json:"token"`
}

func (h *Handler) requireInviteActor(ctx context.Context, orgID, userID string) (*Member, error) {
	m, err := h.inviteStore().GetActiveMemberForUser(ctx, orgID, userID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return nil, errors.NotFoundError("organization", orgID)
		}
		return nil, httpx.MapDB(ctx, err, "failed to load member", httpx.DBErr{})
	}
	return m, nil
}

func inviteConflict(status InviteStatus) error {
	msg := "This invite is no longer valid."
	switch status {
	case InviteStatusRedeemed:
		msg = "This invite has already been used."
	case InviteStatusExpired:
		msg = "This invite has expired."
	}
	return errors.ConflictError(msg, "status", string(status))
}

func (h *Handler) CreateInvite(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")

	actor, err := h.requireInviteActor(ctx, orgID, userID)
	if err != nil {
		return err
	}

	var req CreateInviteRequest
	if len(c.Body()) > 0 {
		if err := c.Bind().Body(&req); err != nil {
			return err
		}
	}

	role, err := ParseRole(req.Role, RoleMember)
	if err != nil {
		return err
	}
	if err := CanCreateMember(actor, role, orgID); err != nil {
		return err
	}

	email, err := ParseInviteEmail(req.Email)
	if err != nil {
		return err
	}
	ttl, err := ParseInviteTTL(req.ExpiresInSeconds)
	if err != nil {
		return err
	}
	maxUses, err := ParseMaxUsesJSON(c.Body())
	if err != nil {
		return err
	}

	plaintext, err := GenerateInviteToken()
	if err != nil {
		httpx.LogError(ctx, "failed to generate invite token", err, errors.KindInternal)
		return errors.InternalError()
	}

	now := time.Now().UTC()
	inv := &Invite{
		ID:             NewULID(),
		OrganizationID: orgID,
		Role:           role,
		Email:          email,
		Token:          &plaintext,
		TokenHash:      HashInviteToken(plaintext),
		TokenPrefix:    InviteDisplayPrefix(plaintext),
		Status:         InviteStatusActive,
		MaxUses:        maxUses,
		UseCount:       0,
		CreatedBy:      userID,
		ExpiresAt:      now.Add(ttl),
		CreatedAt:      now,
	}

	if err := h.inviteStore().CreateInvite(ctx, inv); err != nil {
		return httpx.MapDB(ctx, err, "failed to create invite", httpx.DBErr{})
	}

	metrics.RecordInviteOp("create")
	return c.Status(fiber.StatusCreated).JSON(toInviteResponse(inv, true))
}

func (h *Handler) ListInvites(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")

	actor, err := h.requireInviteActor(ctx, orgID, userID)
	if err != nil {
		return err
	}

	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, last, err := h.inviteStore().ListInvites(ctx, orgID, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list invites", httpx.DBErr{})
	}

	includeToken := actor.Role == RoleAdmin || actor.Role == RoleOwner
	out := ListInvitesResponse{
		Invites: make([]InviteResponse, 0, len(list)),
		Last:    last,
	}
	for _, inv := range list {
		out.Invites = append(out.Invites, toInviteResponse(inv, includeToken))
	}
	return c.JSON(out)
}

func (h *Handler) RevokeInvite(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	inviteID := c.Params("invite_id")

	actor, err := h.requireInviteActor(ctx, orgID, userID)
	if err != nil {
		return err
	}
	if err := RequireAdminOrOwner(actor, "invite.revoke", "invite", inviteID); err != nil {
		return err
	}
	if inviteID == "" {
		return errors.FieldError("invite_id", errors.FallbackValidation)
	}

	_, err = h.inviteStore().RevokeInvite(ctx, orgID, inviteID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to revoke invite", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "invite", ResourceID: inviteID,
		})
	}

	metrics.RecordInviteOp("revoke")
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) RedeemInvite(c fiber.Ctx) error {
	userID := middleware.GetUserID(c)

	var req RedeemInviteRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	return h.redeem(c, strings.TrimSpace(req.Token), userID)
}

func (h *Handler) redeem(c fiber.Ctx, token, userID string) error {
	ctx := c.Context()
	if token == "" || !LooksLikeInviteToken(token) {
		return errors.NotFoundError("invite", nil)
	}

	member, err := h.inviteStore().RedeemInvite(ctx, HashInviteToken(token), userID)
	if err != nil {
		var dead *InviteDeadError
		if stderrors.As(err, &dead) {
			return inviteConflict(dead.Status)
		}
		return httpx.MapDB(ctx, err, "failed to redeem invite", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "invite", ResourceID: nil,
		})
	}

	metrics.RecordInviteOp("redeem")
	metrics.RecordMemberOp("create")
	return c.JSON(toMemberResponse(member))
}

func toInviteResponse(inv *Invite, includeToken bool) InviteResponse {
	out := InviteResponse{
		ID:             inv.ID,
		OrganizationID: inv.OrganizationID,
		Role:           string(inv.Role),
		Email:          inv.Email,
		TokenPrefix:    inv.TokenPrefix,
		Status:         string(inv.Status),
		MaxUses:        inv.MaxUses,
		UseCount:       inv.UseCount,
		ExpiresAt:      httpx.FormatTime(inv.ExpiresAt),
		CreatedBy:      inv.CreatedBy,
		CreatedAt:      httpx.FormatTime(inv.CreatedAt),
	}
	if includeToken && inv.Token != nil && inv.Status == InviteStatusActive {
		out.Token = *inv.Token
	}
	if out.Status == "" {
		out.Status = string(InviteStatusActive)
	}
	return out
}
