package orgs

import "github.com/gofiber/fiber/v3"

// MountPublic registers organization, member, invite, and service-account routes.
// Caller should attach auth middleware on the group (e.g. RequireUserID).
func (h *Handler) MountPublic(router fiber.Router) {
	router.Post("/", h.CreateOrganization)
	router.Get("/", h.ListOrganizations)
	router.Get("/:organization_id", h.GetOrganization)
	router.Patch("/:organization_id", h.UpdateOrganization)
	router.Delete("/:organization_id", h.DeleteOrganization)

	router.Get("/:organization_id/members", h.ListMembers)
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

// MountInternal registers resolve and invite redeem on a router already scoped under /internal
// (or full path if mounted at app root with path included by caller).
func (h *Handler) MountInternal(router fiber.Router) {
	router.Post("/members/resolve", h.Resolve)
	router.Post("/invites/redeem", h.RedeemInviteInternal)
}
