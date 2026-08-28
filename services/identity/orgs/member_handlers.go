package orgs

import (
	stderrors "errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/middleware"
)

type CreateMemberRequest struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

type UpdateMemberRequest struct {
	Role   *string `json:"role"`
	Status *string `json:"status"`
}

type MemberResponse struct {
	ID               string  `json:"id"`
	OrganizationID   string  `json:"organization_id"`
	Principal        string  `json:"principal"`
	UserID           *string `json:"user_id"`
	ServiceAccountID *string `json:"service_account_id"`
	Role             string  `json:"role"`
	Status           string  `json:"status"`
	AddedBy          *string `json:"added_by"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
}

type ListMembersResponse struct {
	Members []MemberResponse `json:"members"`
	Last    *string          `json:"last"`
}

type ResolveRequest struct {
	UserID         string `json:"user_id"`
	OrganizationID string `json:"organization_id"`
}

type ResolveResponse struct {
	MemberID       string `json:"member_id"`
	OrganizationID string `json:"organization_id"`
	UserID         string `json:"user_id"`
	Status         string `json:"status"`
}

func (h *Handler) ListMembers(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, last, err := h.store.ListMembers(ctx, orgID, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list members", httpx.DBErr{})
	}

	out := ListMembersResponse{
		Members: make([]MemberResponse, 0, len(list)),
		Last:    last,
	}
	for _, m := range list {
		out.Members = append(out.Members, toMemberResponse(m))
	}
	return c.JSON(out)
}

func (h *Handler) CreateMember(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
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

	role, err := ParseRole(req.Role, RoleMember)
	if err != nil {
		return err
	}
	if err := CanCreateMember(actor, role, orgID); err != nil {
		return err
	}

	now := time.Now().UTC()
	addedBy := userID
	m := &Member{
		ID:             NewULID(),
		OrganizationID: orgID,
		UserID:         &targetUser,
		Role:           role,
		Status:         StatusActive,
		AddedBy:        &addedBy,
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
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	m, err := h.store.GetMember(ctx, orgID, memberID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to get member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}
	if m.Status == StatusRemoved {
		return errors.NotFoundError("member", memberID)
	}
	return c.JSON(toMemberResponse(m))
}

func (h *Handler) UpdateMember(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}

	var req UpdateMemberRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	var newRole *Role
	if req.Role != nil {
		role, err := ParseRole(*req.Role, "")
		if err != nil {
			return err
		}
		newRole = &role
	}
	var newStatus *Status
	if req.Status != nil {
		status, err := ParseStatus(*req.Status)
		if err != nil {
			return err
		}
		newStatus = &status
	}

	m, err := h.store.MutateMember(ctx, orgID, memberID, func(m *Member, activeOwners int) error {
		return ApplyMemberUpdate(actor, m, userID, newRole, newStatus, activeOwners)
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
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}

	_, err = h.store.MutateMember(ctx, orgID, memberID, func(m *Member, activeOwners int) error {
		return ApplyMemberRemove(actor, m, userID, activeOwners)
	})
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to remove member", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "member", ResourceID: memberID,
		})
	}

	metrics.RecordMemberOp("remove")
	return c.SendStatus(fiber.StatusNoContent)
}

// Resolve handles POST /internal/members/resolve (internal listener; not gateway-published).
func (h *Handler) Resolve(c fiber.Ctx) error {
	ctx := c.Context()

	var req ResolveRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	userID := strings.TrimSpace(req.UserID)
	orgID := strings.TrimSpace(req.OrganizationID)
	if userID == "" || orgID == "" {
		return errors.ValidationFields(errors.FallbackValidation,
			errors.Field{Path: "user_id", Message: errors.FallbackValidation},
			errors.Field{Path: "organization_id", Message: errors.FallbackValidation},
		)
	}

	m, err := h.store.ResolveMember(ctx, userID, orgID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			metrics.RecordResolve("miss")
			return errors.NotFoundError("member", userID+":"+orgID)
		}
		metrics.RecordResolve("error")
		return httpx.MapDB(ctx, err, "failed to resolve member", httpx.DBErr{})
	}

	metrics.RecordResolve("hit")

	uid := ""
	if m.UserID != nil {
		uid = *m.UserID
	}

	return c.JSON(ResolveResponse{
		MemberID:       m.ID,
		OrganizationID: m.OrganizationID,
		UserID:         uid,
		Status:         string(m.Status),
	})
}

func toMemberResponse(m *Member) MemberResponse {
	return MemberResponse{
		ID:               m.ID,
		OrganizationID:   m.OrganizationID,
		Principal:        m.Principal(),
		UserID:           m.UserID,
		ServiceAccountID: m.ServiceAccountID,
		Role:             string(m.Role),
		Status:           string(m.Status),
		AddedBy:          m.AddedBy,
		CreatedAt:        httpx.FormatTime(m.CreatedAt),
		UpdatedAt:        httpx.FormatTime(m.UpdatedAt),
	}
}
