package members

import (
	"slices"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

// --- users ---

func (h *Handler) LookupUser(c fiber.Ctx) error {
	issuer, subject := strings.TrimSpace(c.Query("issuer")), strings.TrimSpace(c.Query("subject"))
	if issuer == "" {
		return errors.FieldError("issuer", errors.FallbackValidation)
	}
	if subject == "" {
		return errors.FieldError("subject", errors.FallbackValidation)
	}
	u, err := h.store.LookupUser(c.Context(), issuer, subject)
	if err != nil {
		return mapErr(c, err, "failed to look up user", "user", "")
	}
	out := []UserResponse{}
	if u != nil {
		out = append(out, userResponse(u))
	}
	return c.JSON(fiber.Map{"users": out, "has_more": false})
}

func (h *Handler) GetUser(c fiber.Ctx) error {
	userID := httpx.PathParam(c, "user_id")
	u, err := h.store.GetUser(c.Context(), userID)
	if err != nil {
		return mapErr(c, err, "failed to get user", "user", userID)
	}
	return c.JSON(userResponse(u))
}

// SetName is PATCH /users/{user_id} { name }: a string sets the person's
// name, null clears it. It reads no caller header.
func (h *Handler) SetName(c fiber.Ctx) error {
	var req struct {
		Name httpx.PatchField `json:"name"`
	}
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	if !req.Name.Set {
		return errors.FieldError("name", "Nothing to update.")
	}
	name, err := httpx.DecodePatch[string](req.Name, "name")
	if err != nil {
		return err
	}
	userID := httpx.PathParam(c, "user_id")
	before, u, err := h.store.SetName(c.Context(), userID, name)
	if err != nil {
		return mapErr(c, err, "failed to set name", "user", userID)
	}
	d := auditx.Details{}
	d.ChangedPtr("name", before, u.Name)
	auditx.Set(c, d)
	return c.JSON(userResponse(u))
}

func (h *Handler) DeleteAccount(c fiber.Ctx) error {
	userID := httpx.PathParam(c, "user_id")
	changed, err := h.store.DeleteAccount(c.Context(), userID)
	if err != nil {
		return mapErr(c, err, "failed to delete account", "user", userID)
	}
	if changed {
		d := auditx.Details{}
		d.Changed("status", StatusActive, StatusRemoved)
		auditx.Set(c, d)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) SetStatus(c fiber.Ctx) error {
	var req struct {
		Status string `json:"status"`
	}
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	userID := httpx.PathParam(c, "user_id")
	before, u, err := h.store.SetStatus(c.Context(), userID, req.Status)
	if err != nil {
		return mapErr(c, err, "failed to set status", "user", userID)
	}
	d := auditx.Details{}
	d.Changed("status", before, u.Status)
	auditx.Set(c, d)
	return c.JSON(userResponse(u))
}

func (h *Handler) UserNodes(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}
	deleted, err := listStatus(c)
	if err != nil {
		return err
	}
	userID := httpx.PathParam(c, "user_id")
	list, hasMore, err := h.store.ListUserNodes(c.Context(), h.src.Current(), userID, c.Query("kind"), deleted, limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list nodes", "user", userID)
	}
	return h.store.nodes.RespondList(c, list, hasMore)
}

// --- service accounts ---

type CreateServiceAccountRequest struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

func (h *Handler) ListServiceAccounts(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}
	list, hasMore, err := h.store.ListServiceAccounts(c.Context(), httpx.PathParam(c, "node_id"), limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list service accounts", "service_account", "")
	}
	out := make([]ServiceAccountResponse, 0, len(list))
	for _, sa := range list {
		out = append(out, serviceAccountResponse(sa))
	}
	return c.JSON(fiber.Map{"service_accounts": out, "has_more": hasMore})
}

func (h *Handler) CreateServiceAccount(c fiber.Ctx) error {
	var req CreateServiceAccountRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	sa, b, err := h.store.CreateServiceAccount(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), req.Name, req.Role)
	if err != nil {
		return mapErr(c, err, "failed to create service account", "service_account", "")
	}
	auditx.Set(c, auditx.Details{"member_id": sa.ID, "binding_id": b.ID})
	auditx.SetCreated(c, sa.ID)
	return c.Status(fiber.StatusCreated).JSON(serviceAccountResponse(sa))
}

func (h *Handler) GetServiceAccount(c fiber.Ctx) error {
	saID := httpx.PathParam(c, "service_account_id")
	sa, err := h.store.GetServiceAccount(c.Context(), httpx.PathParam(c, "node_id"), saID)
	if err != nil {
		return mapErr(c, err, "failed to get service account", "service_account", saID)
	}
	return c.JSON(serviceAccountResponse(sa))
}

func (h *Handler) RenameServiceAccount(c fiber.Ctx) error {
	var req struct {
		Name *string `json:"name"`
	}
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	if req.Name == nil {
		return errors.FieldError("name", "Nothing to update.")
	}
	saID := httpx.PathParam(c, "service_account_id")
	before, sa, err := h.store.RenameServiceAccount(c.Context(), httpx.PathParam(c, "node_id"), saID, *req.Name)
	if err != nil {
		return mapErr(c, err, "failed to rename service account", "service_account", saID)
	}
	d := auditx.Details{}
	d.Changed("name", before, sa.Name)
	auditx.Set(c, d)
	return c.JSON(serviceAccountResponse(sa))
}

func (h *Handler) DeleteServiceAccount(c fiber.Ctx) error {
	saID := httpx.PathParam(c, "service_account_id")
	changed, err := h.store.DeleteServiceAccount(c.Context(), httpx.PathParam(c, "node_id"), saID)
	if err != nil {
		return mapErr(c, err, "failed to delete service account", "service_account", saID)
	}
	if changed {
		d := auditx.Details{"member_id": saID}
		d.Changed("status", StatusActive, StatusRemoved)
		auditx.Set(c, d)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// --- keys, on a service account or a person ---

func keyOwner(c fiber.Ctx) KeyOwner {
	return KeyOwner{
		NodeID:   httpx.PathParam(c, "node_id"),
		MemberID: httpx.PathParam(c, "service_account_id"),
		UserID:   httpx.PathParam(c, "user_id"),
	}
}

func (h *Handler) ListKeys(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}
	list, hasMore, err := h.store.ListKeys(c.Context(), keyOwner(c), limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list keys", "key", "")
	}
	out := make([]KeyResponse, 0, len(list))
	for _, k := range list {
		out = append(out, keyResponse(k))
	}
	return c.JSON(fiber.Map{"keys": out, "has_more": hasMore})
}

func (h *Handler) CreateKey(c fiber.Ctx) error {
	var req struct {
		Name         string           `json:"name"`
		AllowedCIDRs []string         `json:"allowed_cidrs"`
		Scopes       []string         `json:"scopes"`
		AllowedRoots []string         `json:"allowed_roots"`
		ExpiresAt    httpx.PatchField `json:"expires_at"`
		ExpiresIn    httpx.PatchField `json:"expires_in"`
	}
	if err := httpx.OptionalBody(c, &req); err != nil {
		return err
	}
	_, expiresAt, err := parseExpiry(req.ExpiresAt, req.ExpiresIn, h.store.now().UTC())
	if err != nil {
		return err
	}
	k, err := h.store.CreateKey(c.Context(), keyOwner(c), KeyCreate{
		Name:         req.Name,
		AllowedCIDRs: req.AllowedCIDRs,
		Scopes:       req.Scopes,
		AllowedRoots: req.AllowedRoots,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		return mapErr(c, err, "failed to create key", "key", "")
	}
	auditx.Set(c, auditx.Details{"key_id": k.ID, "key_prefix": k.KeyPrefix})
	auditx.SetCreated(c, k.ID)
	return c.Status(fiber.StatusCreated).JSON(keyResponse(k))
}

func decodeSlicePatch(f httpx.PatchField, field string) (*[]string, error) {
	v, err := httpx.DecodePatch[[]string](f, field)
	if err != nil {
		return nil, err
	}
	if v == nil {
		empty := []string{}
		return &empty, nil
	}
	return v, nil
}

func (h *Handler) PatchKey(c fiber.Ctx) error {
	var req struct {
		Name         httpx.PatchField `json:"name"`
		AllowedCIDRs httpx.PatchField `json:"allowed_cidrs"`
		Scopes       httpx.PatchField `json:"scopes"`
		AllowedRoots httpx.PatchField `json:"allowed_roots"`
		ExpiresAt    httpx.PatchField `json:"expires_at"`
		ExpiresIn    httpx.PatchField `json:"expires_in"`
	}
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	if !req.Name.Set && !req.AllowedCIDRs.Set && !req.Scopes.Set && !req.AllowedRoots.Set && !req.ExpiresAt.Set && !req.ExpiresIn.Set {
		return errors.FieldError("", "Nothing to update.")
	}
	var patch KeyPatch
	if req.Name.Set {
		name, err := httpx.DecodeRequiredPatch[string](req.Name, "name")
		if err != nil {
			return err
		}
		patch.Name = &name
	}
	if req.AllowedCIDRs.Set {
		cidrs, err := decodeSlicePatch(req.AllowedCIDRs, "allowed_cidrs")
		if err != nil {
			return err
		}
		patch.AllowedCIDRs = cidrs
	}
	if req.Scopes.Set {
		scopes, err := decodeSlicePatch(req.Scopes, "scopes")
		if err != nil {
			return err
		}
		patch.Scopes = scopes
	}
	if req.AllowedRoots.Set {
		roots, err := decodeSlicePatch(req.AllowedRoots, "allowed_roots")
		if err != nil {
			return err
		}
		patch.AllowedRoots = roots
	}
	expSet, expVal, err := parseExpiry(req.ExpiresAt, req.ExpiresIn, h.store.now().UTC())
	if err != nil {
		return err
	}
	patch.ExpiresAtSet = expSet
	patch.ExpiresAt = expVal

	keyID := httpx.PathParam(c, "key_id")
	before, after, err := h.store.UpdateKey(c.Context(), keyOwner(c), keyID, patch)
	if err != nil {
		return mapErr(c, err, "failed to update key", "key", keyID)
	}
	d := auditx.Details{}
	d.Changed("name", before.Name, after.Name)
	if !slices.Equal(before.AllowedCIDRs, after.AllowedCIDRs) {
		d["allowed_cidrs"] = auditx.Change{From: before.AllowedCIDRs, To: after.AllowedCIDRs}
	}
	if !slices.Equal(before.Scopes, after.Scopes) {
		d["scopes"] = auditx.Change{From: before.Scopes, To: after.Scopes}
	}
	if !slices.Equal(before.AllowedRoots, after.AllowedRoots) {
		d["allowed_roots"] = auditx.Change{From: before.AllowedRoots, To: after.AllowedRoots}
	}
	d.ChangedPtr("expires_at", httpx.FormatTimePtr(before.ExpiresAt), httpx.FormatTimePtr(after.ExpiresAt))
	auditx.Set(c, d)
	return c.JSON(keyResponse(after))
}

func (h *Handler) RevokeKey(c fiber.Ctx) error {
	keyID := httpx.PathParam(c, "key_id")
	k, changed, err := h.store.RevokeKey(c.Context(), keyOwner(c), keyID)
	if err != nil {
		return mapErr(c, err, "failed to revoke key", "key", keyID)
	}
	if changed {
		d := auditx.Details{"key_id": k.ID, "key_prefix": k.KeyPrefix}
		d.ChangedPtr("revoked_at", nil, httpx.FormatTimePtr(k.RevokedAt))
		auditx.Set(c, d)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
