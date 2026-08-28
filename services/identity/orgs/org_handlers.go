package orgs

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/middleware"
)

type CreateOrgRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type UpdateOrgRequest struct {
	Name *string `json:"name"`
	Slug *string `json:"slug"`
}

type OrgResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type ListOrgsResponse struct {
	Organizations []OrgResponse `json:"organizations"`
	Last          *string       `json:"last"`
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
		return errors.FieldError("slug", "Slug can only use lowercase letters, numbers, and dashes.")
	}

	now := time.Now().UTC()
	org := &Organization{
		ID:        NewULID(),
		Name:      name,
		Slug:      slug,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if _, err := h.store.CreateOrganization(ctx, org, userID); err != nil {
		return httpx.MapDB(ctx, err, "failed to create organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: org.ID,
			Conflict: ErrConflict, Field: "slug", FieldValue: slug,
			Message: "An organization with this slug already exists.",
		})
	}

	metrics.RecordOrgCreated()
	metrics.RecordMemberOp("create_owner")
	return c.Status(fiber.StatusCreated).JSON(toOrgResponse(org))
}

func (h *Handler) ListOrganizations(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, last, err := h.store.ListOrganizationsForUser(ctx, userID, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list organizations", httpx.DBErr{})
	}

	out := ListOrgsResponse{
		Organizations: make([]OrgResponse, 0, len(list)),
		Last:          last,
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
			return errors.FieldError("slug", "Slug can only use lowercase letters, numbers, and dashes.")
		}
		org.Slug = slug
	}

	if err := h.store.UpdateOrganization(ctx, org); err != nil {
		return httpx.MapDB(ctx, err, "failed to update organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
			Conflict: ErrConflict, Field: "slug", FieldValue: org.Slug,
			Message: "An organization with this slug already exists.",
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
	return OrgResponse{
		ID:        o.ID,
		Name:      o.Name,
		Slug:      o.Slug,
		CreatedAt: httpx.FormatTime(o.CreatedAt),
		UpdatedAt: httpx.FormatTime(o.UpdatedAt),
	}
}
