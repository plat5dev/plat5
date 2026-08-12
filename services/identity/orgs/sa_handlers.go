package orgs

import (
	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

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
	ctx, span := h.tracer.Start(c.Context(), "service_accounts.create")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	span.SetAttributes(attribute.String("organization.id", orgID))

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
	invitedBy := userID
	if _, err := h.store.CreateServiceAccount(ctx, sa, RoleMember, &invitedBy); err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to create service account",
			notFound("organization", orgID),
		)
	}

	span.SetAttributes(attribute.String("service_account.id", sa.ID))
	span.SetStatus(codes.Ok, "created")
	return c.Status(fiber.StatusCreated).JSON(toServiceAccountResponse(sa))
}

func (h *Handler) ListServiceAccounts(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "service_accounts.list")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	span.SetAttributes(attribute.String("organization.id", orgID))

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	limit, offset, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}

	list, hasMore, err := h.store.ListServiceAccounts(ctx, orgID, limit, offset)
	if err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to list service accounts", storeErrOpts{})
	}

	out := ListServiceAccountsResponse{
		ServiceAccounts: make([]ServiceAccountResponse, 0, len(list)),
		HasMore:         hasMore,
	}
	for _, sa := range list {
		out.ServiceAccounts = append(out.ServiceAccounts, toServiceAccountResponse(sa))
	}

	span.SetAttributes(attribute.Int("service_accounts.count", len(out.ServiceAccounts)))
	span.SetStatus(codes.Ok, "ok")
	return c.JSON(out)
}

func (h *Handler) GetServiceAccount(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "service_accounts.get")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	saID := c.Params("service_account_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("service_account.id", saID),
	)

	if _, err := h.requireActiveMember(ctx, orgID, userID); err != nil {
		return err
	}

	sa, err := h.store.GetServiceAccount(ctx, orgID, saID)
	if err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to get service account",
			notFound("service_account", saID),
		)
	}

	span.SetStatus(codes.Ok, "ok")
	return c.JSON(toServiceAccountResponse(sa))
}

func (h *Handler) UpdateServiceAccount(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "service_accounts.update")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	saID := c.Params("service_account_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("service_account.id", saID),
	)

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
		return errors.FieldError("body", "at least one of name, disabled required")
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
		return h.mapStoreErr(ctx, span, err, "failed to update service account",
			notFound("service_account", saID),
		)
	}

	span.SetStatus(codes.Ok, "ok")
	return c.JSON(toServiceAccountResponse(sa))
}

func (h *Handler) DeleteServiceAccount(c fiber.Ctx) error {
	ctx, span := h.tracer.Start(c.Context(), "service_accounts.delete")
	defer span.End()

	userID := middleware.GetUserID(c)
	orgID := c.Params("organization_id")
	saID := c.Params("service_account_id")
	span.SetAttributes(
		attribute.String("organization.id", orgID),
		attribute.String("service_account.id", saID),
	)

	actor, err := h.requireActiveMember(ctx, orgID, userID)
	if err != nil {
		return err
	}
	if err := RequireAdminOrOwner(actor, "service_account.delete", "service_account", saID); err != nil {
		return err
	}

	if err := h.store.DeleteServiceAccount(ctx, orgID, saID); err != nil {
		return h.mapStoreErr(ctx, span, err, "failed to delete service account",
			notFound("service_account", saID),
		)
	}

	span.SetStatus(codes.Ok, "ok")
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
