package userkeys

import "github.com/gofiber/fiber/v3"

// MountPublic registers user API key routes. Caller attaches auth on the group.
// Mount under /api/user. The subject is X-User-Id, not a path parameter.
func (h *Handler) MountPublic(router fiber.Router) {
	router.Post("/api-keys", h.Create)
	router.Get("/api-keys", h.List)
	router.Delete("/api-keys/:key_id", h.Revoke)
}

// MountInternal registers validate on a router scoped under /internal.
func (h *Handler) MountInternal(router fiber.Router) {
	router.Post("/user-keys/validate", h.Validate)
}
