package sessions

import "github.com/gofiber/fiber/v3"

// MountPublic registers session mint. Mount under /users.
func (h *Handler) MountPublic(router fiber.Router) {
	router.Post("/:user_id/organizations/:organization_id/session", h.Create)
}

// MountInternal registers validate on a router scoped under /internal.
func (h *Handler) MountInternal(router fiber.Router) {
	router.Post("/member-sessions/validate", h.Validate)
}
