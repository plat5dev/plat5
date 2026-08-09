package orgs

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/organizations/errors"
	"github.com/plat5dev/plat5/organizations/metrics"
	"github.com/plat5dev/plat5/organizations/middleware"
	"github.com/plat5dev/plat5/organizations/telemetry"
)

const tracerName = "organizations.handlers"

type Handler struct {
	store  *Store
	tracer trace.Tracer
	telem  *telemetry.Telemetry
}

func NewHandler(store *Store, telem *telemetry.Telemetry) *Handler {
	return &Handler{
		store:  store,
		tracer: otel.Tracer(tracerName),
		telem:  telem,
	}
}

type CreateOrgRequest struct {
	Name     string          `json:"name"`
	Slug     string          `json:"slug"`
	Settings json.RawMessage `json:"settings"`
}

type UpdateOrgRequest struct {
	Name     *string          `json:"name"`
	Slug     *string          `json:"slug"`
	Settings *json.RawMessage `json:"settings"`
}

type OrgResponse struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Slug      string          `json:"slug"`
	Settings  json.RawMessage `json:"settings"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

type ListOrgsResponse struct {
	Organizations []OrgResponse `json:"organizations"`
	HasMore       bool          `json:"has_more"`
}

type CreateMembershipRequest struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

type UpdateMembershipRequest struct {
	Role   *string `json:"role"`
	Status *string `json:"status"`
}

type MembershipResponse struct {
	ID             string  `json:"id"`
	OrganizationID string  `json:"organization_id"`
	UserID         string  `json:"user_id"`
	Role           string  `json:"role"`
	Status         string  `json:"status"`
	InvitedBy      *string `json:"invited_by"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

type ListMembershipsResponse struct {
	Memberships []MembershipResponse `json:"memberships"`
	HasMore     bool                 `json:"has_more"`
}

type ResolveRequest struct {
	UserID         string `json:"user_id"`
	OrganizationID string `json:"organization_id"`
}

type ResolveResponse struct {
	MembershipID   string `json:"membership_id"`
	OrganizationID string `json:"organization_id"`
	UserID         string `json:"user_id"`
	Status         string `json:"status"`
}

func (h *Handler) CreateOrganization(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "organizations.create")
	defer span.End()

	userID := middleware.GetUserID(c)

	var req CreateOrgRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return errors.ValidationError("Request validation failed", map[string]interface{}{
			"fields": []map[string]string{{"path": "name", "message": "required"}},
		})
	}
	if len(name) > MaxOrgNameLen {
		return errors.ValidationError("Request validation failed", map[string]interface{}{
			"fields": []map[string]string{{"path": "name", "message": "must be at most 128 characters"}},
		})
	}

	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = Slugify(name)
	} else if !ValidSlug(slug) {
		return errors.ValidationError("Request validation failed", map[string]interface{}{
			"fields": []map[string]string{{"path": "slug", "message": "must be lowercase alphanumeric with hyphens"}},
		})
	}

	settings := req.Settings
	if len(settings) == 0 {
		settings = json.RawMessage(`{}`)
	} else if !ValidSettingsObject(settings) {
		return errors.ValidationError("Request validation failed", map[string]interface{}{
			"fields": []map[string]string{{"path": "settings", "message": "must be a JSON object"}},
		})
	}

	now := time.Now().UTC()
	org := &Organization{
		ID:        NewULID(),
		Name:      name,
		Slug:      slug,
		Settings:  settings,
		CreatedAt: now,
		UpdatedAt: now,
	}
	span.SetAttributes(attribute.String("organization.id", org.ID))

	membership, err := h.store.CreateOrganization(ctx, org, userID)
	if err != nil {
		if stderrors.Is(err, ErrConflict) {
			return errors.ConflictError("slug", slug)
		}
		h.logError(ctx, span, "failed to create organization", err, errors.KindDB)
		return errors.InternalError()
	}

	metrics.RecordOrgCreated()
	metrics.RecordMembershipOp("create_owner")
	span.SetAttributes(attribute.String("membership.id", membership.ID))
	span.SetStatus(codes.Ok, "created")

	return c.Status(fiber.StatusCreated).JSON(toOrgResponse(org))
}

func (h *Handler) ListOrganizations(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "organizations.list")
	defer span.End()

	userID := middleware.GetUserID(c)
	limit, offset, err := parseListParams(c)
	if err != nil {
		return err
	}

	orgs, hasMore, err := h.store.ListOrganizationsForUser(ctx, userID, limit, offset)
	if err != nil {
		h.logError(ctx, span, "failed to list organizations", err, errors.KindDB)
		return errors.InternalError()
	}

	out := ListOrgsResponse{
		Organizations: make([]OrgResponse, 0, len(orgs)),
		HasMore:       hasMore,
	}
	for _, o := range orgs {
		out.Organizations = append(out.Organizations, toOrgResponse(o))
	}
	span.SetAttributes(attribute.Int("organizations.count", len(out.Organizations)))
	span.SetStatus(codes.Ok, "ok")
	return c.JSON(out)
}

func (h *Handler) GetOrganization(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "organizations.get")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	span.SetAttributes(attribute.String("organization.id", orgID))

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	org, err := h.store.GetOrganization(ctx, orgID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return errors.NotFoundError("organization", orgID)
		}
		h.logError(ctx, span, "failed to get organization", err, errors.KindDB)
		return errors.InternalError()
	}

	span.SetStatus(codes.Ok, "ok")
	return c.JSON(toOrgResponse(org))
}

func (h *Handler) UpdateOrganization(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "organizations.update")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	span.SetAttributes(attribute.String("organization.id", orgID))

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}
	if actor.Role != RoleAdmin && actor.Role != RoleOwner {
		return errors.ForbiddenError("organization.update", "organization", orgID)
	}

	var req UpdateOrgRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	org, err := h.store.GetOrganization(ctx, orgID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return errors.NotFoundError("organization", orgID)
		}
		h.logError(ctx, span, "failed to get organization", err, errors.KindDB)
		return errors.InternalError()
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return errors.ValidationError("Request validation failed", map[string]interface{}{
				"fields": []map[string]string{{"path": "name", "message": "required"}},
			})
		}
		if len(name) > MaxOrgNameLen {
			return errors.ValidationError("Request validation failed", map[string]interface{}{
				"fields": []map[string]string{{"path": "name", "message": "must be at most 128 characters"}},
			})
		}
		org.Name = name
	}
	if req.Slug != nil {
		slug := strings.TrimSpace(*req.Slug)
		if !ValidSlug(slug) {
			return errors.ValidationError("Request validation failed", map[string]interface{}{
				"fields": []map[string]string{{"path": "slug", "message": "must be lowercase alphanumeric with hyphens"}},
			})
		}
		org.Slug = slug
	}
	if req.Settings != nil {
		if !ValidSettingsObject(*req.Settings) {
			return errors.ValidationError("Request validation failed", map[string]interface{}{
				"fields": []map[string]string{{"path": "settings", "message": "must be a JSON object"}},
			})
		}
		org.Settings = *req.Settings
	}

	if err := h.store.UpdateOrganization(ctx, org); err != nil {
		if stderrors.Is(err, ErrConflict) {
			return errors.ConflictError("slug", org.Slug)
		}
		if stderrors.Is(err, ErrNotFound) {
			return errors.NotFoundError("organization", orgID)
		}
		h.logError(ctx, span, "failed to update organization", err, errors.KindDB)
		return errors.InternalError()
	}

	span.SetStatus(codes.Ok, "ok")
	return c.JSON(toOrgResponse(org))
}

func (h *Handler) DeleteOrganization(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "organizations.delete")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	span.SetAttributes(attribute.String("organization.id", orgID))

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}
	if actor.Role != RoleOwner {
		return errors.ForbiddenError("organization.delete", "organization", orgID)
	}

	if err := h.store.DeleteOrganization(ctx, orgID); err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return errors.NotFoundError("organization", orgID)
		}
		h.logError(ctx, span, "failed to delete organization", err, errors.KindDB)
		return errors.InternalError()
	}

	span.SetStatus(codes.Ok, "ok")
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) ListMemberships(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "memberships.list")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	span.SetAttributes(attribute.String("organization.id", orgID))

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	limit, offset, err := parseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.ListMemberships(ctx, orgID, limit, offset)
	if err != nil {
		h.logError(ctx, span, "failed to list memberships", err, errors.KindDB)
		return errors.InternalError()
	}

	out := ListMembershipsResponse{
		Memberships: make([]MembershipResponse, 0, len(list)),
		HasMore:     hasMore,
	}
	for _, m := range list {
		out.Memberships = append(out.Memberships, toMembershipResponse(m))
	}
	span.SetAttributes(attribute.Int("memberships.count", len(out.Memberships)))
	span.SetStatus(codes.Ok, "ok")
	return c.JSON(out)
}

func (h *Handler) CreateMembership(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "memberships.create")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	span.SetAttributes(attribute.String("organization.id", orgID))

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}
	if actor.Role != RoleAdmin && actor.Role != RoleOwner {
		return errors.ForbiddenError("membership.create", "organization", orgID)
	}

	var req CreateMembershipRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	targetUser := strings.TrimSpace(req.UserID)
	if targetUser == "" {
		return errors.ValidationError("Request validation failed", map[string]interface{}{
			"fields": []map[string]string{{"path": "user_id", "message": "required"}},
		})
	}
	if len(targetUser) > MaxUserIDLen {
		return errors.ValidationError("Request validation failed", map[string]interface{}{
			"fields": []map[string]string{{"path": "user_id", "message": "must be at most 128 characters"}},
		})
	}

	role := Role(strings.TrimSpace(req.Role))
	if role == "" {
		role = RoleMember
	}
	if !role.Valid() {
		return errors.ValidationError("Request validation failed", map[string]interface{}{
			"fields": []map[string]string{{"path": "role", "message": "must be member, admin, or owner"}},
		})
	}
	if role == RoleOwner && actor.Role != RoleOwner {
		return errors.ForbiddenError("membership.create_owner", "organization", orgID)
	}

	now := time.Now().UTC()
	invitedBy := userID
	m := &Membership{
		ID:             NewULID(),
		OrganizationID: orgID,
		UserID:         targetUser,
		Role:           role,
		Status:         StatusActive,
		InvitedBy:      &invitedBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := h.store.CreateMembership(ctx, m); err != nil {
		if stderrors.Is(err, ErrConflict) {
			return errors.ConflictError("user_id", targetUser)
		}
		h.logError(ctx, span, "failed to create membership", err, errors.KindDB)
		return errors.InternalError()
	}

	metrics.RecordMembershipOp("create")
	span.SetAttributes(attribute.String("membership.id", m.ID))
	span.SetStatus(codes.Ok, "created")
	return c.Status(fiber.StatusCreated).JSON(toMembershipResponse(m))
}

func (h *Handler) GetMembership(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "memberships.get")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	membershipID := c.Params("membership_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("membership.id", membershipID),
	)

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	m, err := h.store.GetMembership(ctx, orgID, membershipID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return errors.NotFoundError("membership", membershipID)
		}
		h.logError(ctx, span, "failed to get membership", err, errors.KindDB)
		return errors.InternalError()
	}
	if m.Status == StatusRemoved {
		return errors.NotFoundError("membership", membershipID)
	}

	span.SetStatus(codes.Ok, "ok")
	return c.JSON(toMembershipResponse(m))
}

func (h *Handler) UpdateMembership(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "memberships.update")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	membershipID := c.Params("membership_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("membership.id", membershipID),
	)

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}

	var req UpdateMembershipRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	m, err := h.store.MutateMembership(ctx, orgID, membershipID, func(m *Membership, activeOwners int) error {
		isSelf := m.UserID == userID

		if req.Role != nil {
			if actor.Role != RoleAdmin && actor.Role != RoleOwner {
				return errors.ForbiddenError("membership.update_role", "membership", membershipID)
			}
			// Only owners may change another owner's role.
			if m.Role == RoleOwner && actor.Role != RoleOwner {
				return errors.ForbiddenError("membership.manage_owner", "membership", membershipID)
			}
			newRole := Role(strings.TrimSpace(*req.Role))
			if !newRole.Valid() {
				return errors.ValidationError("Request validation failed", map[string]interface{}{
					"fields": []map[string]string{{"path": "role", "message": "must be member, admin, or owner"}},
				})
			}
			if newRole == RoleOwner && actor.Role != RoleOwner {
				return errors.ForbiddenError("membership.promote_owner", "membership", membershipID)
			}
			if m.Role == RoleOwner && newRole != RoleOwner && activeOwners <= 1 {
				return errors.ValidationError("Cannot demote the sole owner", map[string]interface{}{
					"fields": []map[string]string{{"path": "role", "message": "sole owner cannot be demoted"}},
				})
			}
			m.Role = newRole
		}

		if req.Status != nil {
			newStatus := Status(strings.TrimSpace(*req.Status))
			if !newStatus.Valid() {
				return errors.ValidationError("Request validation failed", map[string]interface{}{
					"fields": []map[string]string{{"path": "status", "message": "invalid status"}},
				})
			}
			if newStatus == StatusRemoved && isSelf {
				if m.Role == RoleOwner && activeOwners <= 1 {
					return errors.ValidationError("Cannot leave as sole owner", map[string]interface{}{
						"fields": []map[string]string{{"path": "status", "message": "transfer ownership first"}},
					})
				}
			} else {
				if actor.Role != RoleAdmin && actor.Role != RoleOwner {
					return errors.ForbiddenError("membership.update_status", "membership", membershipID)
				}
				// Only owners may change another owner's status.
				if m.Role == RoleOwner && actor.Role != RoleOwner {
					return errors.ForbiddenError("membership.manage_owner", "membership", membershipID)
				}
			}
			if m.Role == RoleOwner && newStatus != StatusActive && m.Status == StatusActive && activeOwners <= 1 {
				return errors.ValidationError("Cannot change status of sole owner", map[string]interface{}{
					"fields": []map[string]string{{"path": "status", "message": "sole owner must remain active"}},
				})
			}
			m.Status = newStatus
		}
		return nil
	})
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return errors.NotFoundError("membership", membershipID)
		}
		if _, ok := err.(*errors.ApiError); ok {
			return err
		}
		h.logError(ctx, span, "failed to update membership", err, errors.KindDB)
		return errors.InternalError()
	}

	metrics.RecordMembershipOp("update")
	span.SetStatus(codes.Ok, "ok")
	return c.JSON(toMembershipResponse(m))
}

func (h *Handler) DeleteMembership(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "memberships.delete")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	membershipID := c.Params("membership_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("membership.id", membershipID),
	)

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}

	_, err = h.store.MutateMembership(ctx, orgID, membershipID, func(m *Membership, activeOwners int) error {
		isSelf := m.UserID == userID
		if !isSelf && actor.Role != RoleAdmin && actor.Role != RoleOwner {
			return errors.ForbiddenError("membership.remove", "membership", membershipID)
		}
		// Only owners may remove another owner.
		if m.Role == RoleOwner && !isSelf && actor.Role != RoleOwner {
			return errors.ForbiddenError("membership.manage_owner", "membership", membershipID)
		}
		if m.Role == RoleOwner && activeOwners <= 1 {
			return errors.ValidationError("Cannot remove the sole owner", map[string]interface{}{
				"fields": []map[string]string{{"path": "membership_id", "message": "transfer ownership first"}},
			})
		}
		m.Status = StatusRemoved
		return nil
	})
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return errors.NotFoundError("membership", membershipID)
		}
		if _, ok := err.(*errors.ApiError); ok {
			return err
		}
		h.logError(ctx, span, "failed to remove membership", err, errors.KindDB)
		return errors.InternalError()
	}

	metrics.RecordMembershipOp("remove")
	span.SetStatus(codes.Ok, "ok")
	return c.SendStatus(fiber.StatusNoContent)
}

// Resolve handles POST /internal/memberships/resolve (internal listener; not gateway-published).
func (h *Handler) Resolve(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "memberships.resolve")
	defer span.End()

	var req ResolveRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	userID := strings.TrimSpace(req.UserID)
	orgID := strings.TrimSpace(req.OrganizationID)
	if userID == "" || orgID == "" {
		return errors.ValidationError("Request validation failed", map[string]interface{}{
			"fields": []map[string]string{
				{"path": "user_id", "message": "required"},
				{"path": "organization_id", "message": "required"},
			},
		})
	}

	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("organization.id", orgID),
	)

	m, err := h.store.ResolveMembership(ctx, userID, orgID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			metrics.RecordResolve("miss")
			span.SetStatus(codes.Ok, "miss")
			return errors.NotFoundError("membership", userID+":"+orgID)
		}
		h.logError(ctx, span, "failed to resolve membership", err, errors.KindDB)
		metrics.RecordResolve("error")
		return errors.InternalError()
	}

	metrics.RecordResolve("hit")
	span.SetAttributes(
		attribute.String("membership.id", m.ID),
		attribute.String("membership.status", string(m.Status)),
	)
	span.SetStatus(codes.Ok, "hit")

	return c.JSON(ResolveResponse{
		MembershipID:   m.ID,
		OrganizationID: m.OrganizationID,
		UserID:         m.UserID,
		Status:         string(m.Status),
	})
}

func parseListParams(c fiber.Ctx) (limit, offset int, err error) {
	limit = DefaultListLimit
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil || n < 1 {
			return 0, 0, errors.ValidationError("Request validation failed", map[string]interface{}{
				"fields": []map[string]string{{"path": "limit", "message": "must be a positive integer"}},
			})
		}
		limit = n
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	if v := strings.TrimSpace(c.Query("offset")); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil || n < 0 {
			return 0, 0, errors.ValidationError("Request validation failed", map[string]interface{}{
				"fields": []map[string]string{{"path": "offset", "message": "must be a non-negative integer"}},
			})
		}
		offset = n
	}
	return limit, offset, nil
}

// requireActiveMember returns 404 for non-member / unknown org (existence policy).
func (h *Handler) requireActiveMember(ctx context.Context, orgID, userID string) (*Membership, error) {
	m, err := h.store.GetActiveMembership(ctx, orgID, userID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return nil, errors.NotFoundError("organization", orgID)
		}
		h.logError(ctx, nil, "failed to load membership", err, errors.KindDB)
		return nil, errors.InternalError()
	}
	return m, nil
}

func toOrgResponse(o *Organization) OrgResponse {
	settings := json.RawMessage(o.Settings)
	if len(settings) == 0 {
		settings = json.RawMessage(`{}`)
	}
	return OrgResponse{
		ID:        o.ID,
		Name:      o.Name,
		Slug:      o.Slug,
		Settings:  settings,
		CreatedAt: o.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: o.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func toMembershipResponse(m *Membership) MembershipResponse {
	return MembershipResponse{
		ID:             m.ID,
		OrganizationID: m.OrganizationID,
		UserID:         m.UserID,
		Role:           string(m.Role),
		Status:         string(m.Status),
		InvitedBy:      m.InvitedBy,
		CreatedAt:      m.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      m.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (h *Handler) logError(ctx context.Context, span trace.Span, msg string, err error, kind errors.ErrorKind) {
	if span != nil {
		span.SetStatus(codes.Error, msg)
		span.SetAttributes(
			attribute.String("error.kind", kind.String()),
			attribute.String("error.message", err.Error()),
		)
		span.RecordError(err)
	}

	logger := h.telem.LoggerWithContext(ctx)
	logger.Error().
		Str("error_kind", kind.String()).
		Str("error_message", err.Error()).
		Msg(msg)
}
