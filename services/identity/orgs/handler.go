package orgs

import (
	"context"
	"strings"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

type Handler struct {
	store   *Store
	invites inviteStore
}

func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
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
