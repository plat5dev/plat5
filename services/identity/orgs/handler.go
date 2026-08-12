package orgs

import (
	"context"
	stderrors "errors"
	"strings"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

type Handler struct {
	store *Store
}

func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

// RequireActiveMember returns the active membership for (org, user).
// Unknown org, non-member, or non-active → 404 organization (existence policy).
// Unexpected store errors are returned as-is for MapDB.
func RequireActiveMember(ctx context.Context, store *Store, orgID, userID string) (*Member, error) {
	m, err := store.GetActiveMemberForUser(ctx, orgID, userID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return nil, errors.NotFoundError("organization", orgID)
		}
		return nil, err
	}
	return m, nil
}

func (h *Handler) requireActiveMember(ctx context.Context, orgID, userID string) (*Member, error) {
	m, err := RequireActiveMember(ctx, h.store, orgID, userID)
	if err != nil {
		return nil, httpx.MapDB(ctx, err, "failed to load member", httpx.DBErr{})
	}
	return m, nil
}

func requireName(raw, path string, maxLen int) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.FieldError(path, "required")
	}
	if len(name) > maxLen {
		return "", errors.FieldError(path, "must be at most 128 characters")
	}
	return name, nil
}
