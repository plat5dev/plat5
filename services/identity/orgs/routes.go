package orgs

import "github.com/gofiber/fiber/v3"

// MountUser registers person routes. Mount under /users.
func (h *Handler) MountUser(router fiber.Router) {
	router.Get("/:user_id/memberships", h.ListMemberships)
	router.Post("/:user_id/organizations", h.CreateOrganization)
	router.Post("/:user_id/invites/redeem", h.RedeemInvite)
}

// MountOrganizations registers org routes on the application root.
func (h *Handler) MountOrganizations(router fiber.Router) {
	router.Get("/organizations", h.ListOrganizations)
	router.Get("/organizations/:organization_id", h.GetOrganization)
	router.Patch("/organizations/:organization_id", h.UpdateOrganization)
	router.Delete("/organizations/:organization_id", h.DeleteOrganization)

	router.Get("/organizations/:organization_id/members", h.ListMembers)
	router.Post("/organizations/:organization_id/members", h.CreateMember)

	router.Get("/organizations/:organization_id/invites", h.ListInvites)
	router.Post("/organizations/:organization_id/invites", h.CreateInvite)
	router.Delete("/organizations/:organization_id/invites/:invite_id", h.RevokeInvite)

	router.Post("/organizations/:organization_id/service-accounts", h.CreateServiceAccount)
	router.Get("/organizations/:organization_id/service-accounts", h.ListServiceAccounts)
	router.Get("/organizations/:organization_id/service-accounts/:service_account_id", h.GetServiceAccount)
	router.Patch("/organizations/:organization_id/service-accounts/:service_account_id", h.UpdateServiceAccount)
	router.Delete("/organizations/:organization_id/service-accounts/:service_account_id", h.DeleteServiceAccount)
}

// MountMembers registers member routes. Mount under /members.
func (h *Handler) MountMembers(router fiber.Router) {
	router.Get("/:member_id", h.GetMember)
	router.Patch("/:member_id", h.UpdateMember)
	router.Delete("/:member_id", h.DeleteMember)
}
