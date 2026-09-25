package orgs

import (
	"context"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
)

type CreateMemberRequest struct {
	UserID  string  `json:"user_id"`
	AddedBy *string `json:"added_by"`
}

type UpdateMemberRequest struct {
	Status *string `json:"status"`
}

type MemberResponse struct {
	ID               string  `json:"id"`
	OrganizationID   string  `json:"organization_id"`
	Principal        string  `json:"principal"`
	UserID           *string `json:"user_id"`
	ServiceAccountID *string `json:"service_account_id"`
	Status           string  `json:"status"`
	AddedBy          *string `json:"added_by"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
}

type ListMembersResponse struct {
	Members []MemberResponse `json:"members"`
	HasMore bool             `json:"has_more"`
}

func (h *Handler) ListMembers(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")

	if err := h.requireOrganization(ctx, orgID); err != nil {
		return err
	}

	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.ListMembers(ctx, orgID, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list members", httpx.DBErr{})
	}

	out := ListMembersResponse{
		Members: make([]MemberResponse, 0, len(list)),
		HasMore: hasMore,
	}
	for _, m := range list {
		out.Members = append(out.Members, toMemberResponse(m))
	}
	return c.JSON(out)
}

func (h *Handler) CreateMember(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")

	if err := h.requireOrganization(ctx, orgID); err != nil {
		return err
	}

	var req CreateMemberRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	targetUser := strings.TrimSpace(req.UserID)
	if targetUser == "" {
		return errors.FieldError("user_id", "Choose someone to add.")
	}
	if len(targetUser) > MaxUserIDLen {
		return errors.FieldError("user_id", "That user ID is too long.")
	}
	addedBy, err := optionalUserID(req.AddedBy, "added_by")
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	m := &Member{
		ID:             NewULID(),
		OrganizationID: orgID,
		UserID:         &targetUser,
		Status:         StatusActive,
		AddedBy:        addedBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := h.store.CreateUserMember(ctx, m); err != nil {
		return httpx.MapDB(ctx, err, "failed to create member", httpx.DBErr{
			Conflict: ErrConflict, Field: "user_id", FieldValue: targetUser,
			Message: "This person is already a member.",
		})
	}

	metrics.RecordMemberOp("create")
	return c.Status(fiber.StatusCreated).JSON(toMemberResponse(m))
}

func (h *Handler) GetMember(c fiber.Ctx) error {
	ctx := c.Context()
	memberID := httpx.PathParam(c, "member_id")

	m, err := h.visibleMember(ctx, memberID)
	if err != nil {
		return err
	}
	return c.JSON(toMemberResponse(m))
}

func (h *Handler) UpdateMember(c fiber.Ctx) error {
	ctx := c.Context()
	memberID := httpx.PathParam(c, "member_id")

	var req UpdateMemberRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	if req.Status == nil {
		return errors.FieldError("status", "Status is required.")
	}
	status, err := ParsePatchStatus(*req.Status)
	if err != nil {
		return err
	}

	m, err := h.store.MutateMember(ctx, memberID, func(m *Member, _ int) error {
		m.Status = status
		return nil
	})
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to update member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}

	metrics.RecordMemberOp("update")
	return c.JSON(toMemberResponse(m))
}

func (h *Handler) DeleteMember(c fiber.Ctx) error {
	ctx := c.Context()
	memberID := httpx.PathParam(c, "member_id")

	_, err := h.store.MutateMember(ctx, memberID, func(m *Member, nonRemoved int) error {
		if err := rejectLastMember(nonRemoved, "member_id"); err != nil {
			return err
		}
		m.Status = StatusRemoved
		return nil
	})
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to remove member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}

	metrics.RecordMemberOp("remove")
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) visibleMember(ctx context.Context, memberID string) (*Member, error) {
	m, err := h.store.GetMember(ctx, memberID)
	if err != nil {
		return nil, httpx.MapDB(ctx, err, "failed to get member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}
	if m.Status == StatusRemoved {
		return nil, errors.NotFoundError("member", memberID)
	}
	return m, nil
}

func toMemberResponse(m *Member) MemberResponse {
	return MemberResponse{
		ID:               m.ID,
		OrganizationID:   m.OrganizationID,
		Principal:        m.Principal(),
		UserID:           m.UserID,
		ServiceAccountID: m.ServiceAccountID,
		Status:           string(m.Status),
		AddedBy:          m.AddedBy,
		CreatedAt:        httpx.FormatTime(m.CreatedAt),
		UpdatedAt:        httpx.FormatTime(m.UpdatedAt),
	}
}
