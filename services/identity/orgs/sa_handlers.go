package orgs

import (
	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

type CreateServiceAccountRequest struct {
	Name string  `json:"name"`
	Role *string `json:"role"`
}

type UpdateServiceAccountRequest struct {
	Name *string `json:"name"`
}

type ServiceAccountResponse struct {
	ID             string  `json:"id"`
	OrganizationID string  `json:"organization_id"`
	MemberID       string  `json:"member_id"`
	Name           string  `json:"name"`
	Role           *string `json:"role"`
	Status         string  `json:"status"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

type ListServiceAccountsResponse struct {
	ServiceAccounts []ServiceAccountResponse `json:"service_accounts"`
	HasMore         bool                     `json:"has_more"`
}

func (h *Handler) CreateServiceAccount(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")

	var req CreateServiceAccountRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	name, err := requireName(req.Name, "name", MaxSANameLen)
	if err != nil {
		return err
	}
	role, err := h.roles.ChooseServiceAccount(req.Role)
	if err != nil {
		return err
	}

	sa := &ServiceAccount{
		ID:             NewULID(),
		OrganizationID: orgID,
		Name:           name,
		Role:           role,
	}
	if _, err := h.store.CreateServiceAccount(ctx, sa); err != nil {
		return httpx.MapDB(ctx, err, "failed to create service account", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "organization", ResourceID: orgID,
		})
	}
	return c.Status(fiber.StatusCreated).JSON(h.toServiceAccountResponse(sa))
}

func (h *Handler) ListServiceAccounts(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")

	if err := h.requireOrganization(ctx, orgID); err != nil {
		return err
	}

	limit, startingAfter, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.ListServiceAccounts(ctx, orgID, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list service accounts", httpx.DBErr{})
	}

	out := ListServiceAccountsResponse{
		ServiceAccounts: make([]ServiceAccountResponse, 0, len(list)),
		HasMore:         hasMore,
	}
	for _, sa := range list {
		out.ServiceAccounts = append(out.ServiceAccounts, h.toServiceAccountResponse(sa))
	}
	return c.JSON(out)
}

func (h *Handler) GetServiceAccount(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")
	saID := c.Params("service_account_id")

	sa, err := h.store.GetServiceAccount(ctx, orgID, saID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to get service account", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "service_account", ResourceID: saID,
		})
	}
	return c.JSON(h.toServiceAccountResponse(sa))
}

func (h *Handler) UpdateServiceAccount(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")
	saID := c.Params("service_account_id")

	var req UpdateServiceAccountRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	if req.Name == nil {
		return errors.FieldError("body", "Nothing to update.")
	}
	name, err := requireName(*req.Name, "name", MaxSANameLen)
	if err != nil {
		return err
	}
	sa, err := h.store.UpdateServiceAccount(ctx, orgID, saID, name)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to update service account", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "service_account", ResourceID: saID,
		})
	}
	return c.JSON(h.toServiceAccountResponse(sa))
}

func (h *Handler) DeleteServiceAccount(c fiber.Ctx) error {
	ctx := c.Context()
	orgID := httpx.PathParam(c, "organization_id")
	saID := c.Params("service_account_id")

	err := h.store.DeleteServiceAccount(ctx, orgID, saID, func(target *Member, members []*Member) error {
		if err := rejectLastMember(countNonRemoved(members), "service_account_id"); err != nil {
			return err
		}
		prior := *target
		target.Status = StatusRemoved
		return rejectLastCreator(h.roles, members, prior, "service_account_id")
	})
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to delete service account", httpx.DBErr{
			NotFound: ErrNotFound, Resource: "service_account", ResourceID: saID,
		})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) toServiceAccountResponse(sa *ServiceAccount) ServiceAccountResponse {
	return ServiceAccountResponse{
		ID:             sa.ID,
		OrganizationID: sa.OrganizationID,
		MemberID:       sa.MemberID,
		Name:           sa.Name,
		Role:           sa.Role,
		Status:         string(sa.Status),
		CreatedAt:      httpx.FormatTime(sa.CreatedAt),
		UpdatedAt:      httpx.FormatTime(sa.UpdatedAt),
	}
}
