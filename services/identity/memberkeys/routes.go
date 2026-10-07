package memberkeys

import (
	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/internal/httpx"
)

// MountPublic registers member API key routes. Mount under /members.
// Those are the self address. MountServiceAccountKeys is the org address for
// the same rows.
func (h *Handler) MountPublic(router fiber.Router) {
	router.Post("/:member_id/api-keys", h.Create)
	router.Get("/:member_id/api-keys", h.List)
	router.Delete("/:member_id/api-keys/:key_id", h.Revoke)
}

// MountServiceAccountKeys registers the org address for a service account's
// member keys. Mount on the application root. Create and revoke manage the org
// and refuse restricted credentials (httpx.RequireUnrestricted).
func (h *Handler) MountServiceAccountKeys(router fiber.Router) {
	router.Post("/organizations/:organization_id/service-accounts/:service_account_id/api-keys", httpx.RequireUnrestricted, h.CreateForServiceAccount)
	router.Get("/organizations/:organization_id/service-accounts/:service_account_id/api-keys", h.ListForServiceAccount)
	router.Delete("/organizations/:organization_id/service-accounts/:service_account_id/api-keys/:key_id", httpx.RequireUnrestricted, h.RevokeForServiceAccount)
}

// MountInternal registers validate on a router scoped under /internal.
func (h *Handler) MountInternal(router fiber.Router) {
	router.Post("/member-keys/validate", h.Validate)
}
