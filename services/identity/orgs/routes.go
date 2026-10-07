package orgs

import (
	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/internal/httpx"
)

// MountUser registers person routes. Mount under /users.
func (h *Handler) MountUser(router fiber.Router) {
	router.Get("/:user_id/memberships", h.ListMemberships)
	router.Post("/:user_id/organizations", h.CreateOrganization)
	router.Post("/:user_id/invites/redeem", h.RedeemInvite)
}

// MountOrganizations registers org routes on the application root.
// Writes that manage the org refuse restricted credentials (httpx.RequireUnrestricted).
func (h *Handler) MountOrganizations(router fiber.Router) {
	manage := httpx.RequireUnrestricted

	router.Get("/organizations", h.ListOrganizations)
	router.Get("/organizations/:organization_id", h.GetOrganization)
	router.Patch("/organizations/:organization_id", manage, h.UpdateOrganization)
	router.Delete("/organizations/:organization_id", manage, h.DeleteOrganization)

	router.Get("/organizations/:organization_id/members", h.ListMembers)
	router.Post("/organizations/:organization_id/members", manage, h.CreateMember)

	router.Get("/organizations/:organization_id/invites", h.ListInvites)
	router.Post("/organizations/:organization_id/invites", manage, h.CreateInvite)
	router.Delete("/organizations/:organization_id/invites/:invite_id", manage, h.RevokeInvite)

	router.Post("/organizations/:organization_id/service-accounts", manage, h.CreateServiceAccount)
	router.Get("/organizations/:organization_id/service-accounts", h.ListServiceAccounts)
	router.Get("/organizations/:organization_id/service-accounts/:service_account_id", h.GetServiceAccount)
	router.Patch("/organizations/:organization_id/service-accounts/:service_account_id", manage, h.UpdateServiceAccount)
	router.Delete("/organizations/:organization_id/service-accounts/:service_account_id", manage, h.DeleteServiceAccount)
}

// MountMembers registers member routes. Mount under /members.
// Status changes and removal manage the org and refuse restricted credentials.
func (h *Handler) MountMembers(router fiber.Router) {
	router.Get("/:member_id", h.GetMember)
	router.Patch("/:member_id", httpx.RequireUnrestricted, h.UpdateMember)
	router.Delete("/:member_id", httpx.RequireUnrestricted, h.DeleteMember)
}
