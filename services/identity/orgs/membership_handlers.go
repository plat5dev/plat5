package orgs

import (
	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/internal/httpx"
)

type MembershipOrgResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type MembershipResponse struct {
	ID           string                `json:"id"`
	Organization MembershipOrgResponse `json:"organization"`
	Status       string                `json:"status"`
}

type ListMembershipsResponse struct {
	Memberships []MembershipResponse `json:"memberships"`
	HasMore     bool                 `json:"has_more"`
}

func (h *Handler) ListMemberships(c fiber.Ctx) error {
	ctx := c.Context()
	userID := httpx.PathParam(c, "user_id")
	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.ListMemberships(ctx, userID, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list memberships", httpx.DBErr{})
	}

	out := ListMembershipsResponse{
		Memberships: make([]MembershipResponse, 0, len(list)),
		HasMore:     hasMore,
	}
	for _, m := range list {
		out.Memberships = append(out.Memberships, toMembershipResponse(m))
	}
	return c.JSON(out)
}

func toMembershipResponse(m *Membership) MembershipResponse {
	return MembershipResponse{
		ID: m.ID,
		Organization: MembershipOrgResponse{
			ID:   m.OrganizationID,
			Name: m.OrganizationName,
			Slug: m.OrganizationSlug,
		},
		Status: string(m.Status),
	}
}
