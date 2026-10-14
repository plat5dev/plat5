package orgs

import (
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
)

type CreateOrgRequest struct {
	Name string `json:"name"`
}

type UpdateOrgRequest struct {
	Name *string `json:"name"`
}

type OrgResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
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

	now := time.Now().UTC()
	org := &Organization{
		ID:        NewULID(),
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}

	creator, err := h.store.CreateOrganization(ctx, org, userID, h.roles.Set().Creator())
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to create organization", httpx.DBErr{
			NotFound: ErrUserNotFound, Resource: "user", ResourceID: userID,
		})
	}

	metrics.RecordOrgCreated()
	metrics.RecordMemberOp("create")
	auditx.SetCreated(c, org.ID)
	auditx.SetOrganization(c, org.ID, creator.ID)
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

	var name *string
	if req.Name != nil {
		n, err := requireName(*req.Name, "name", MaxOrgNameLen)
		if err != nil {
			return err
		}
		name = &n
	}

	org, prior, err := h.store.UpdateOrganization(ctx, orgID, name)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to update organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
		})
	}
	details := auditx.Details{}
	details.Changed("name", prior.Name, org.Name)
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
		CreatedAt: httpx.FormatTime(o.CreatedAt),
		UpdatedAt: httpx.FormatTime(o.UpdatedAt),
	}
}

// DeleteUser deletes the person's account. It lives here because it removes
// the person's members, under the same rules as a member remove.
func (h *Handler) DeleteUser(c fiber.Ctx) error {
	ctx := c.Context()
	userID := httpx.PathParam(c, "user_id")

	if err := h.store.DeleteAccount(ctx, userID, h.roles.Set()); err != nil {
		return httpx.MapDB(ctx, err, "failed to delete account", httpx.DBErr{
			NotFound: ErrUserNotFound, Resource: "user", ResourceID: userID,
		})
	}
	return c.SendStatus(fiber.StatusNoContent)
}
