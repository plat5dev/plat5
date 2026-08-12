package orgs

import (
	stderrors "errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

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
	InvitedBy        *string `json:"invited_by"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
}

type ListMembersResponse struct {
	Members []MemberResponse `json:"members"`
	HasMore bool             `json:"has_more"`
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
	ctx, span := h.tracer.Start(c.Context(), "members.list")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	span.SetAttributes(attribute.String("organization.id", orgID))

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	limit, offset, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.ListMembers(ctx, orgID, limit, offset)
	if err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to list members", storeErrOpts{})
	}

	out := ListMembersResponse{
		Members: make([]MemberResponse, 0, len(list)),
		HasMore: hasMore,
	}
	for _, m := range list {
		out.Members = append(out.Members, toMemberResponse(m))
	}
	span.SetAttributes(attribute.Int("members.count", len(out.Members)))
	span.SetStatus(codes.Ok, "ok")
	return c.JSON(out)
}

func (h *Handler) CreateMember(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "members.create")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	span.SetAttributes(attribute.String("organization.id", orgID))

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
		return errors.FieldError("user_id", "required")
	}
	if len(targetUser) > MaxUserIDLen {
		return errors.FieldError("user_id", "must be at most 128 characters")
	}

	role, err := ParseRole(req.Role, RoleMember)
	if err != nil {
		return err
	}
	if err := CanCreateMember(actor, role, orgID); err != nil {
		return err
	}

	now := time.Now().UTC()
	invitedBy := userID
	m := &Member{
		ID:             NewULID(),
		OrganizationID: orgID,
		UserID:         &targetUser,
		Role:           role,
		Status:         StatusActive,
		InvitedBy:      &invitedBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := h.store.CreateUserMember(ctx, m); err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to create member",
			conflict("user_id", targetUser),
		)
	}

	metrics.RecordMemberOp("create")
	span.SetAttributes(attribute.String("member.id", m.ID))
	span.SetStatus(codes.Ok, "created")
	return c.Status(fiber.StatusCreated).JSON(toMemberResponse(m))
}

func (h *Handler) GetMember(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "members.get")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("member.id", memberID),
	)

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	m, err := h.store.GetMember(ctx, orgID, memberID)
	if err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to get member",
			notFound("member", memberID),
		)
	}
	if m.Status == StatusRemoved {
		return errors.NotFoundError("member", memberID)
	}

	span.SetStatus(codes.Ok, "ok")
	return c.JSON(toMemberResponse(m))
}

func (h *Handler) UpdateMember(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "members.update")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("member.id", memberID),
	)

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
		return h.mapStoreErr(ctx, span, err, "failed to update member",
			notFound("member", memberID),
		)
	}

	metrics.RecordMemberOp("update")
	span.SetStatus(codes.Ok, "ok")
	return c.JSON(toMemberResponse(m))
}

func (h *Handler) DeleteMember(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "members.delete")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	memberID := c.Params("member_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("member.id", memberID),
	)

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}

	_, err = h.store.MutateMember(ctx, orgID, memberID, func(m *Member, activeOwners int) error {
		return ApplyMemberRemove(actor, m, userID, activeOwners)
	})
	if err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to remove member",
			notFound("member", memberID),
		)
	}

	metrics.RecordMemberOp("remove")
	span.SetStatus(codes.Ok, "ok")
	return c.SendStatus(fiber.StatusNoContent)
}

// Resolve handles POST /internal/members/resolve (internal listener; not gateway-published).
func (h *Handler) Resolve(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "members.resolve")
	defer span.End()

	var req ResolveRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	userID := strings.TrimSpace(req.UserID)
	orgID := strings.TrimSpace(req.OrganizationID)
	if userID == "" || orgID == "" {
		return errors.ValidationFields("Request validation failed",
			errors.Field{Path: "user_id", Message: "required"},
			errors.Field{Path: "organization_id", Message: "required"},
		)
	}

	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("organization.id", orgID),
	)

	m, err := h.store.ResolveMember(ctx, userID, orgID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			metrics.RecordResolve("miss")
			span.SetStatus(codes.Ok, "miss")
			return errors.NotFoundError("member", userID+":"+orgID)
		}
		metrics.RecordResolve("error")
		return h.mapStoreErr(ctx, span, err, "failed to resolve member", storeErrOpts{})
	}

	metrics.RecordResolve("hit")
	span.SetAttributes(
		attribute.String("member.id", m.ID),
		attribute.String("member.status", string(m.Status)),
	)
	span.SetStatus(codes.Ok, "hit")

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
		InvitedBy:        m.InvitedBy,
		CreatedAt:        httpx.FormatTime(m.CreatedAt),
		UpdatedAt:        httpx.FormatTime(m.UpdatedAt),
	}
}
