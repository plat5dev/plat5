package userkeys

import "github.com/gofiber/fiber/v3"

// MountPublic registers user API key routes. Caller attaches auth on the group.
func (h *Handler) MountPublic(router fiber.Router) {
	router.Post("/:user_id/api-keys", h.Create)
	router.Get("/:user_id/api-keys", h.List)
	router.Delete("/:user_id/api-keys/:key_id", h.Revoke)
}

// MountInternal registers validate on a router scoped under /internal.
func (h *Handler) MountInternal(router fiber.Router) {
	router.Post("/user-keys/validate", h.Validate)
}
