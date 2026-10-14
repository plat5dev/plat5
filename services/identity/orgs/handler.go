package orgs

import (
	"context"
	"strings"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/roles"
)

// people resolves an IdP subject to a person in the directory, creating them
// when the subject is new.
type people interface {
	Resolve(ctx context.Context, issuer, subject string) (userID, status string, err error)
}

type Handler struct {
	store   *Store
	invites inviteStore
	people  people
	// roles is the deployment's roles file. Nil: roles are off.
	roles *roles.Source
}

func NewHandler(store *Store, dir people, roleSource *roles.Source) *Handler {
	return &Handler{store: store, people: dir, roles: roleSource}
}

func (h *Handler) requireOrganization(ctx context.Context, orgID string) error {
	if _, err := h.store.GetOrganization(ctx, orgID); err != nil {
		return httpx.MapDB(ctx, err, "failed to get organization", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
		})
	}
	return nil
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
