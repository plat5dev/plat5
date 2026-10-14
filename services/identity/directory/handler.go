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
	List(ctx context.Context, limit int, startingAfter string) ([]*User, bool, error)
	Get(ctx context.Context, userID string) (*User, error)
	Update(ctx context.Context, userID string, u Update) (*User, User, error)
}

type Handler struct {
	store userStore
}

func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

// UpdateRequest is the person's own fields. Status is not one: it has its own
// route, which the customer catalog leaves out, so a person cannot suspend
// themselves and be locked out.
type UpdateRequest struct {
	DisplayName httpx.Nullable `json:"display_name"`
	Status      *string        `json:"status"`
}

type StatusRequest struct {
	Status *string `json:"status"`
}

type UserResponse struct {
	ID          string  `json:"id"`
	DisplayName *string `json:"display_name"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

type ListResponse struct {
	Users   []UserResponse `json:"users"`
	HasMore bool           `json:"has_more"`
}

// List is every person. Not in the customer catalog: staff list people through
// the operator gateway.
func (h *Handler) List(c fiber.Ctx) error {
	ctx := c.Context()
	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}
	list, hasMore, err := h.store.List(ctx, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list users", httpx.DBErr{})
	}
	out := ListResponse{Users: make([]UserResponse, 0, len(list)), HasMore: hasMore}
	for _, u := range list {
		out.Users = append(out.Users, toUserResponse(u))
	}
	return c.JSON(out)
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
	userID := httpx.PathParam(c, "user_id")

	var req UpdateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	if req.Status != nil {
		return errors.FieldError("status", "Status is changed at /users/{user_id}/status.")
	}
	if !req.DisplayName.Set {
		return errors.FieldError("body", "Nothing to update.")
	}
	name, clear, err := ParseDisplayName(req.DisplayName, "display_name")
	if err != nil {
		return err
	}
	return h.update(c, userID, Update{DisplayName: name, ClearName: clear})
}

// SetStatus suspends or re-enables a person. Not in the customer catalog:
// staff call it through the operator gateway.
func (h *Handler) SetStatus(c fiber.Ctx) error {
	userID := httpx.PathParam(c, "user_id")

	var req StatusRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	if req.Status == nil {
		return errors.FieldError("status", "Status is required.")
	}
	status := strings.TrimSpace(*req.Status)
	if status != StatusActive && status != StatusSuspended {
		return errors.FieldError("status", "Status must be active or suspended.")
	}
	return h.update(c, userID, Update{Status: &status})
}

func (h *Handler) update(c fiber.Ctx, userID string, u Update) error {
	ctx := c.Context()
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

// Mount registers person routes. Mount under /users. GET / and
// PATCH /:user_id/status are for staff: the customer catalog omits them. DELETE is account
// deletion, which is the orgs package's: it removes the person's members.
func (h *Handler) Mount(router fiber.Router) {
	router.Get("/", h.List)
	router.Get("/:user_id", h.Get)
	router.Patch("/:user_id", h.Update)
	router.Patch("/:user_id/status", h.SetStatus)
}
