package orgs

import (
	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/internal/httpx"
)

type RoleResponse struct {
	Slug string `json:"slug"`
	// Labels is the file's list as written. ["*"] is every label.
	Labels []string `json:"labels"`
}

type ListRolesResponse struct {
	Roles []RoleResponse `json:"roles"`
	// The three role fields are null when roles are off.
	CreatorRole *string `json:"creator_role"`
	DefaultRole *string `json:"default_role"`
	// ServiceAccountDefaultRole is resolved: default_role when the file leaves it unset.
	ServiceAccountDefaultRole *string `json:"service_account_default_role"`
}

// ListRoles is the deployment's roles file. The org is in the path so a later
// per-org role set has an address; today every org gets the same list.
// Not paginated: the file is the whole list.
func (h *Handler) ListRoles(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")

	if err := h.requireOrganization(ctx, orgID); err != nil {
		return err
	}

	list := h.roles.List()
	out := ListRolesResponse{
		Roles:       make([]RoleResponse, 0, len(list)),
		CreatorRole: h.roles.Creator(),
		DefaultRole: h.roles.Default(),

		ServiceAccountDefaultRole: h.roles.ServiceAccountDefault(),
	}
	for _, r := range list {
		out.Roles = append(out.Roles, RoleResponse{Slug: r.Slug, Labels: r.Labels})
	}
	return c.JSON(out)
}
