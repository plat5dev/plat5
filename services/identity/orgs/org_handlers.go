package orgs

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
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
	HasMore       bool          `json:"has_more"`
}

func (h *Handler) CreateOrganization(c fiber.Ctx) error {
	ctx := c.Context()
	userID := httpx.PathParam(c, "user_id")

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

	if _, err := h.store.CreateOrganization(ctx, org, userID, h.roles.Creator()); err != nil {
		return httpx.MapDB(ctx, err, "failed to create organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: org.ID,
			Conflict: ErrConflict, Field: "slug", FieldValue: slug,
			Message: "An organization with this slug already exists.",
		})
	}

	metrics.RecordOrgCreated()
	metrics.RecordMemberOp("create")
	return c.Status(fiber.StatusCreated).JSON(toOrgResponse(org))
}

func (h *Handler) ListOrganizations(c fiber.Ctx) error {
	ctx := c.Context()
	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.ListOrganizations(ctx, limit, startingAfter)
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
	orgID := httpx.PathParam(c, "organization_id")

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
	orgID := httpx.PathParam(c, "organization_id")

	var req UpdateOrgRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}

	var name, slug *string
	if req.Name != nil {
		n, err := requireName(*req.Name, "name", MaxOrgNameLen)
		if err != nil {
			return err
		}
		name = &n
	}
	if req.Slug != nil {
		s := strings.TrimSpace(*req.Slug)
		if !ValidSlug(s) {
			return errors.FieldError("slug", "Slug can only use lowercase letters, numbers, and dashes.")
		}
		slug = &s
	}

	org, prior, err := h.store.UpdateOrganization(ctx, orgID, name, slug)
	if err != nil {
		var attempted string
		if slug != nil {
			attempted = *slug
		}
		return httpx.MapDB(ctx, err, "failed to update organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
			Conflict: ErrConflict, Field: "slug", FieldValue: attempted,
			Message: "An organization with this slug already exists.",
		})
	}
	details := auditx.Details{}
	details.Changed("name", prior.Name, org.Name)
	details.Changed("slug", prior.Slug, org.Slug)
	auditx.Set(c, details)
	return c.JSON(toOrgResponse(org))
}

func (h *Handler) DeleteOrganization(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")

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
