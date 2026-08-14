package orgs

import (
	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/middleware"
)

type CreateServiceAccountRequest struct {
	Name string `json:"name"`
}

type UpdateServiceAccountRequest struct {
	Name     *string `json:"name"`
	Disabled *bool   `json:"disabled"`
}

type ServiceAccountResponse struct {
	ID              string  `json:"id"`
	OrganizationID  string  `json:"organization_id"`
	MemberID        string  `json:"member_id"`
	Name            string  `json:"name"`
	DisabledAt      *string `json:"disabled_at"`
	CreatedByUserID *string `json:"created_by_user_id"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

type ListServiceAccountsResponse struct {
	ServiceAccounts []ServiceAccountResponse `json:"service_accounts"`
	HasMore         bool                     `json:"has_more"`
}

func (h *Handler) CreateServiceAccount(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}
	if err := RequireAdminOrOwner(actor, "service_account.create", "organization", orgID); err != nil {
		return err
	}

	var req CreateServiceAccountRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	name, err := requireName(req.Name, "name", MaxSANameLen)
	if err != nil {
		return err
	}

	sa := &ServiceAccount{
		ID:              NewULID(),
		OrganizationID:  orgID,
		Name:            name,
		CreatedByUserID: &userID,
	}
	addedBy := userID
	if _, err := h.store.CreateServiceAccount(ctx, sa, RoleMember, &addedBy); err != nil {
		return httpx.MapDB(ctx, err, "failed to create service account", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
		})
	}
	return c.Status(fiber.StatusCreated).JSON(toServiceAccountResponse(sa))
}

func (h *Handler) ListServiceAccounts(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	limit, offset, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.ListServiceAccounts(ctx, orgID, limit, offset)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list service accounts", httpx.DBErr{})
	}

	out := ListServiceAccountsResponse{
		ServiceAccounts: make([]ServiceAccountResponse, 0, len(list)),
		HasMore:         hasMore,
	}
	for _, sa := range list {
		out.ServiceAccounts = append(out.ServiceAccounts, toServiceAccountResponse(sa))
	}
	return c.JSON(out)
}

func (h *Handler) GetServiceAccount(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	saID := c.Params("service_account_id")

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	sa, err := h.store.GetServiceAccount(ctx, orgID, saID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to get service account", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "service_account", ResourceID: saID,
		})
	}
	return c.JSON(toServiceAccountResponse(sa))
}

func (h *Handler) UpdateServiceAccount(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	saID := c.Params("service_account_id")

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}
	if err := RequireAdminOrOwner(actor, "service_account.update", "service_account", saID); err != nil {
		return err
	}

	var req UpdateServiceAccountRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	if req.Name == nil && req.Disabled == nil {
		return errors.FieldError("body", "Nothing to update.")
	}
	var name *string
	if req.Name != nil {
		n, err := requireName(*req.Name, "name", MaxSANameLen)
		if err != nil {
			return err
		}
		name = &n
	}

	sa, err := h.store.UpdateServiceAccount(ctx, orgID, saID, name, req.Disabled)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to update service account", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "service_account", ResourceID: saID,
		})
	}
	return c.JSON(toServiceAccountResponse(sa))
}

func (h *Handler) DeleteServiceAccount(c fiber.Ctx) error {
	ctx := c.Context()
	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	saID := c.Params("service_account_id")

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}
	if err := RequireAdminOrOwner(actor, "service_account.delete", "service_account", saID); err != nil {
		return err
	}

	if err := h.store.DeleteServiceAccount(ctx, orgID, saID); err != nil {
		return httpx.MapDB(ctx, err, "failed to delete service account", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "service_account", ResourceID: saID,
		})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func toServiceAccountResponse(sa *ServiceAccount) ServiceAccountResponse {
	return ServiceAccountResponse{
		ID:              sa.ID,
		OrganizationID:  sa.OrganizationID,
		MemberID:        sa.MemberID,
		Name:            sa.Name,
		DisabledAt:      httpx.FormatTimePtr(sa.DisabledAt),
		CreatedByUserID: sa.CreatedByUserID,
		CreatedAt:       httpx.FormatTime(sa.CreatedAt),
		UpdatedAt:       httpx.FormatTime(sa.UpdatedAt),
	}
}
