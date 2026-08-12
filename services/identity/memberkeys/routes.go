package memberkeys

import "github.com/gofiber/fiber/v3"

// MountPublic registers member API key routes on the organizations group.
// Caller attaches auth on the group.
func (h *Handler) MountPublic(router fiber.Router) {
	router.Post("/:organization_id/members/:member_id/api-keys", h.Create)
	router.Get("/:organization_id/members/:member_id/api-keys", h.List)
	router.Delete("/:organization_id/members/:member_id/api-keys/:key_id", h.Revoke)
}

// MountInternal registers validate on a router scoped under /internal.
func (h *Handler) MountInternal(router fiber.Router) {
	router.Post("/member-keys/validate", h.Validate)
}
