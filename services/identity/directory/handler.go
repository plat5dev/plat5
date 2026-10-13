package directory

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

type userStore interface {
	Get(ctx context.Context, userID string) (*User, error)
	Update(ctx context.Context, userID string, u Update) (*User, User, error)
}

type Handler struct {
	store userStore
}

func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

type UpdateRequest struct {
	DisplayName httpx.Nullable `json:"display_name"`
	Status      *string        `json:"status"`
}

type UserResponse struct {
	ID          string  `json:"id"`
	DisplayName *string `json:"display_name"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

func (h *Handler) Get(c fiber.Ctx) error {
	ctx := c.Context()
	userID := httpx.PathParam(c, "user_id")
	u, err := h.store.Get(ctx, userID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to get user", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "user", ResourceID: userID,
		})
	}
	return c.JSON(toUserResponse(u))
}

func (h *Handler) Update(c fiber.Ctx) error {
	ctx := c.Context()
	userID := httpx.PathParam(c, "user_id")

	var req UpdateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	if !req.DisplayName.Set && req.Status == nil {
		return errors.FieldError("body", "Nothing to update.")
	}
	var u Update
	if req.DisplayName.Set {
		name, clear, err := ParseDisplayName(req.DisplayName, "display_name")
		if err != nil {
			return err
		}
		u.DisplayName, u.ClearName = name, clear
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if status != StatusActive && status != StatusSuspended {
			return errors.FieldError("status", "Status must be active or suspended.")
		}
		u.Status = &status
	}

	user, prior, err := h.store.Update(ctx, userID, u)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to update user", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "user", ResourceID: userID,
		})
	}
	details := auditx.Details{}
	details.Changed("status", prior.Status, user.Status)
	details.ChangedPtr("display_name", prior.DisplayName, user.DisplayName)
	auditx.Set(c, details)
	return c.JSON(toUserResponse(user))
}

// ParseDisplayName reads a display name field that is set: a name, or null
// to clear it. An empty name is refused; null is how to clear it.
func ParseDisplayName(n httpx.Nullable, path string) (name *string, clear bool, err error) {
	if n.Value == nil {
		return nil, true, nil
	}
	s := strings.TrimSpace(*n.Value)
	if s == "" {
		return nil, false, errors.FieldError(path, "Display name can't be empty. Send null to clear it.")
	}
	if len(s) > MaxDisplayNameLen {
		return nil, false, errors.FieldError(path, "Display name is too long.")
	}
	return &s, false, nil
}

func toUserResponse(u *User) UserResponse {
	return UserResponse{
		ID:          u.ID,
		DisplayName: u.DisplayName,
		Status:      u.Status,
		CreatedAt:   httpx.FormatTime(u.CreatedAt),
		UpdatedAt:   httpx.FormatTime(u.UpdatedAt),
	}
}

// Mount registers person routes. Mount under /users. DELETE is account
// deletion, which is the orgs package's: it removes the person's members.
func (h *Handler) Mount(router fiber.Router) {
	router.Get("/:user_id", h.Get)
	router.Patch("/:user_id", h.Update)
}
