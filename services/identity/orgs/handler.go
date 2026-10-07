package orgs

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/roles"
)

type Handler struct {
	store   *Store
	invites inviteStore
	// roles is the deployment's roles file. Nil: no roles, every member unrestricted.
	roles *roles.Set
}

func NewHandler(store *Store, roleSet *roles.Set) *Handler {
	return &Handler{store: store, roles: roleSet}
}

// callerScopes is the caller's effective scopes for the grant cap. nil is unrestricted.
// A malformed header is a platform bug: 500.
func callerScopes(c fiber.Ctx) ([]string, error) {
	caller, err := httpx.CallerScopes(c)
	if err != nil {
		return nil, httpx.MapMintScopes(c.Context(), err)
	}
	return caller, nil
}

func (h *Handler) requireOrganization(ctx context.Context, orgID string) error {
	if _, err := h.store.GetOrganization(ctx, orgID); err != nil {
		return httpx.MapDB(ctx, err, "failed to get organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
		})
	}
	return nil
}

func optionalUserID(raw *string, path string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	id := strings.TrimSpace(*raw)
	if id == "" {
		return nil, nil
	}
	if len(id) > MaxUserIDLen {
		return nil, errors.FieldError(path, "That user ID is too long.")
	}
	return &id, nil
}

func requireName(raw, path string, maxLen int) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.FieldError(path, "Name is required.")
	}
	if len(name) > maxLen {
		return "", errors.FieldError(path, "Name is too long.")
	}
	return name, nil
}
