package orgs

import (
	"encoding/json"
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

func (h *Handler) CreateOrganization(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "organizations.create")
	defer span.End()

	userID := middleware.GetUserID(c)

	var req CreateOrgRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	name, err := requireName(req.Name, "name", MaxOrgNameLen)
	if err != nil {
		return err
	}

	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = Slugify(name)
	} else if !ValidSlug(slug) {
		return errors.FieldError("slug", "must be lowercase alphanumeric with hyphens")
	}

	settings := req.Settings
	if len(settings) == 0 {
		settings = json.RawMessage(`{}`)
	} else if !ValidSettingsObject(settings) {
		return errors.FieldError("settings", "must be a JSON object")
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

	member, err := h.store.CreateOrganization(ctx, org, userID)
	if err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to create organization",
			notFound("organization", org.ID),
			conflict("slug", slug),
		)
	}

	metrics.RecordOrgCreated()
	metrics.RecordMemberOp("create_owner")
	span.SetAttributes(attribute.String("member.id", member.ID))
	span.SetStatus(codes.Ok, "created")

	return c.Status(fiber.StatusCreated).JSON(toOrgResponse(org))
}

func (h *Handler) ListOrganizations(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "organizations.list")
	defer span.End()

	userID := middleware.GetUserID(c)
	limit, offset, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.ListOrganizationsForUser(ctx, userID, limit, offset)
	if err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to list organizations", storeErrOpts{})
	}

	out := ListOrgsResponse{
		Organizations: make([]OrgResponse, 0, len(list)),
		HasMore:       hasMore,
	}
	for _, o := range list {
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
		return h.mapStoreErr(ctx, span, err, "failed to get organization",
			notFound("organization", orgID),
		)
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
	if err := RequireAdminOrOwner(actor, "organization.update", "organization", orgID); err != nil {
		return err
	}

	var req UpdateOrgRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	org, err := h.store.GetOrganization(ctx, orgID)
	if err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to get organization",
			notFound("organization", orgID),
		)
	}

	if req.Name != nil {
		name, err := requireName(*req.Name, "name", MaxOrgNameLen)
		if err != nil {
			return err
		}
		org.Name = name
	}
	if req.Slug != nil {
		slug := strings.TrimSpace(*req.Slug)
		if !ValidSlug(slug) {
			return errors.FieldError("slug", "must be lowercase alphanumeric with hyphens")
		}
		org.Slug = slug
	}
	if req.Settings != nil {
		if !ValidSettingsObject(*req.Settings) {
			return errors.FieldError("settings", "must be a JSON object")
		}
		org.Settings = *req.Settings
	}

	if err := h.store.UpdateOrganization(ctx, org); err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to update organization",
			notFound("organization", orgID),
			conflict("slug", org.Slug),
		)
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
	if err := RequireOwner(actor, "organization.delete", "organization", orgID); err != nil {
		return err
	}

	if err := h.store.DeleteOrganization(ctx, orgID); err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to delete organization",
			notFound("organization", orgID),
		)
	}

	span.SetStatus(codes.Ok, "ok")
	return c.SendStatus(fiber.StatusNoContent)
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
		CreatedAt: httpx.FormatTime(o.CreatedAt),
		UpdatedAt: httpx.FormatTime(o.UpdatedAt),
	}
}
