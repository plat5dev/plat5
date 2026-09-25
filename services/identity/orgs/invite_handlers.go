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
)

type inviteStore interface {
	OrganizationExists(ctx context.Context, organizationID string) (bool, error)
	CreateInvite(ctx context.Context, inv *Invite) error
	ListInvites(ctx context.Context, organizationID string, limit int, startingAfter string) ([]*Invite, bool, error)
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
	Email            string  `json:"email"`
	ExpiresInSeconds *int    `json:"expires_in_seconds"`
	CreatedBy        *string `json:"created_by"`
}

type InviteResponse struct {
	ID             string  `json:"id"`
	OrganizationID string  `json:"organization_id"`
	Email          *string `json:"email"`
	TokenPrefix    string  `json:"token_prefix"`
	Token          string  `json:"token,omitempty"`
	Status         string  `json:"status"`
	MaxUses        *int    `json:"max_uses"`
	UseCount       int     `json:"use_count"`
	ExpiresAt      string  `json:"expires_at"`
	CreatedBy      *string `json:"created_by"`
	CreatedAt      string  `json:"created_at"`
}

type ListInvitesResponse struct {
	Invites []InviteResponse `json:"invites"`
	HasMore bool             `json:"has_more"`
}

type RedeemInviteRequest struct {
	Token string `json:"token"`
}

func (h *Handler) requireInviteOrg(ctx context.Context, orgID string) error {
	ok, err := h.inviteStore().OrganizationExists(ctx, orgID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to get organization", httpx.DBErr{})
	}
	if !ok {
		return errors.NotFoundError("organization", orgID)
	}
	return nil
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
	orgID := httpx.PathParam(c, "organization_id")

	if err := h.requireInviteOrg(ctx, orgID); err != nil {
		return err
	}

	var req CreateInviteRequest
	if len(c.Body()) > 0 {
		if err := c.Bind().Body(&req); err != nil {
			return err
		}
	}

	createdBy, err := optionalUserID(req.CreatedBy, "created_by")
	if err != nil {
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
		Email:          email,
		Token:          &plaintext,
		TokenHash:      HashInviteToken(plaintext),
		TokenPrefix:    InviteDisplayPrefix(plaintext),
		Status:         InviteStatusActive,
		MaxUses:        maxUses,
		UseCount:       0,
		CreatedBy:      createdBy,
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
	orgID := httpx.PathParam(c, "organization_id")

	if err := h.requireInviteOrg(ctx, orgID); err != nil {
		return err
	}

	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.inviteStore().ListInvites(ctx, orgID, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list invites", httpx.DBErr{})
	}

	out := ListInvitesResponse{
		Invites: make([]InviteResponse, 0, len(list)),
		HasMore: hasMore,
	}
	for _, inv := range list {
		out.Invites = append(out.Invites, toInviteResponse(inv, true))
	}
	return c.JSON(out)
}

func (h *Handler) RevokeInvite(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")
	inviteID := c.Params("invite_id")

	if inviteID == "" {
		return errors.FieldError("invite_id", errors.FallbackValidation)
	}

	_, err := h.inviteStore().RevokeInvite(ctx, orgID, inviteID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to revoke invite", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "invite", ResourceID: inviteID,
		})
	}

	metrics.RecordInviteOp("revoke")
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) RedeemInvite(c fiber.Ctx) error {
	userID := httpx.PathParam(c, "user_id")

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
