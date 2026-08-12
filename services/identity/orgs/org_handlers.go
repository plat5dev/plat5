package orgs

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

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
	ctx := c.Context()
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

	if _, err := h.store.CreateOrganization(ctx, org, userID); err != nil {
		return httpx.MapDB(ctx, err, "failed to create organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: org.ID,
			Conflict: ErrConflict, Field: "slug", FieldValue: slug,
		})
	}

	metrics.RecordOrgCreated()
	metrics.RecordMemberOp("create_owner")
	return c.Status(fiber.StatusCreated).JSON(toOrgResponse(org))
}

func (h *Handler) ListOrganizations(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	limit, offset, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.ListOrganizationsForUser(ctx, userID, limit, offset)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list organizations", httpx.DBErr{})
	}

	out := ListOrgsResponse{
		Organizations: make([]OrgResponse, 0, len(list)),
		HasMore:       hasMore,
	}
	for _, o := range list {
		out.Organizations = append(out.Organizations, toOrgResponse(o))
	}
	return c.JSON(out)
}

func (h *Handler) GetOrganization(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	org, err := h.store.GetOrganization(ctx, orgID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to get organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
		})
	}
	return c.JSON(toOrgResponse(org))
}

func (h *Handler) UpdateOrganization(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")

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
		return httpx.MapDB(ctx, err, "failed to get organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
		})
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
		return httpx.MapDB(ctx, err, "failed to update organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
			Conflict: ErrConflict, Field: "slug", FieldValue: org.Slug,
		})
	}
	return c.JSON(toOrgResponse(org))
}

func (h *Handler) DeleteOrganization(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}
	if err := RequireOwner(actor, "organization.delete", "organization", orgID); err != nil {
		return err
	}

	if err := h.store.DeleteOrganization(ctx, orgID); err != nil {
		return httpx.MapDB(ctx, err, "failed to delete organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
		})
	}
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
