package orgs

import "github.com/gofiber/fiber/v3"

// MountDirectory registers reads that do not use the caller.
// Do not attach RequireUserID.
func (h *Handler) MountDirectory(router fiber.Router) {
	router.Get("/organizations", h.ListOrganizations)
	router.Get("/organizations/:organization_id/members", h.ListMembers)
}

// MountMemberships registers GET /memberships. Caller attaches auth on the group.
// Mount under /api/user.
func (h *Handler) MountMemberships(router fiber.Router) {
	router.Get("/memberships", h.ListMemberships)
}

// MountPublic registers organization, member, invite, and service-account routes
// that use the caller. Caller should attach auth middleware on the group.
func (h *Handler) MountPublic(router fiber.Router) {
	router.Post("/", h.CreateOrganization)
	router.Get("/:organization_id", h.GetOrganization)
	router.Patch("/:organization_id", h.UpdateOrganization)
	router.Delete("/:organization_id", h.DeleteOrganization)

	router.Post("/:organization_id/members", h.CreateMember)
	router.Get("/:organization_id/members/:member_id", h.GetMember)
	router.Patch("/:organization_id/members/:member_id", h.UpdateMember)
	router.Delete("/:organization_id/members/:member_id", h.DeleteMember)

	router.Get("/:organization_id/invites", h.ListInvites)
	router.Post("/:organization_id/invites", h.CreateInvite)
	router.Delete("/:organization_id/invites/:invite_id", h.RevokeInvite)

	router.Post("/:organization_id/service-accounts", h.CreateServiceAccount)
	router.Get("/:organization_id/service-accounts", h.ListServiceAccounts)
	router.Get("/:organization_id/service-accounts/:service_account_id", h.GetServiceAccount)
	router.Patch("/:organization_id/service-accounts/:service_account_id", h.UpdateServiceAccount)
	router.Delete("/:organization_id/service-accounts/:service_account_id", h.DeleteServiceAccount)
}

// MountRedeem registers POST /redeem on a router scoped under /api/invites.
func (h *Handler) MountRedeem(router fiber.Router) {
	router.Post("/redeem", h.RedeemInvite)
}

// MountInternal registers member resolve on a router scoped under /internal.
func (h *Handler) MountInternal(router fiber.Router) {
	router.Post("/members/resolve", h.Resolve)
}
