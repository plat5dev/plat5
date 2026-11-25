package members

import (
	"slices"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

func (h *Handler) Roles(c fiber.Ctx) error {
	snap := h.src.Current()
	all := false
	switch c.Query("all") {
	case "", "false":
	case "true":
		all = true
	default:
		return errors.FieldError("all", errors.FallbackValidation)
	}
	roles, err := h.store.ListRoles(c.Context(), snap, httpx.PathParam(c, "node_id"), all)
	if err != nil {
		return mapErr(c, err, "failed to list roles", "node", "")
	}
	out := RolesResponse{Roles: make([]RoleResponse, 0, len(roles)), CreatorRole: snap.CreatorRole}
	for _, r := range roles {
		out.Roles = append(out.Roles, roleResponse(r))
	}
	return c.JSON(out)
}

type CreateRoleRequest struct {
	DisplayName string   `json:"display_name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
	BindableAt  []string `json:"bindable_at"`
}

type PatchRoleRequest struct {
	DisplayName *string   `json:"display_name"`
	Description *string   `json:"description"`
	Permissions *[]string `json:"permissions"`
	BindableAt  *[]string `json:"bindable_at"`
}

func (h *Handler) CreateRole(c fiber.Ctx) error {
	var req CreateRoleRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	r, err := h.store.CreateCustomRole(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), CustomRoleInput{
		DisplayName: req.DisplayName,
		Description: req.Description,
		Permissions: req.Permissions,
		BindableAt:  req.BindableAt,
	})
	if err != nil {
		return mapErr(c, err, "failed to create role", "role", "")
	}
	auditx.Set(c, auditx.Details{"role_id": r.ID})
	auditx.SetCreated(c, r.ID)
	return c.Status(fiber.StatusCreated).JSON(roleResponse(*r))
}

func (h *Handler) GetRole(c fiber.Ctx) error {
	roleID := httpx.PathParam(c, "role_id")
	r, err := h.store.GetRole(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), roleID)
	if err != nil {
		return mapErr(c, err, "failed to get role", "role", roleID)
	}
	return c.JSON(roleResponse(*r))
}

func (h *Handler) PatchRole(c fiber.Ctx) error {
	var req PatchRoleRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	roleID := httpx.PathParam(c, "role_id")
	before, after, err := h.store.PatchCustomRole(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), roleID, CustomRolePatch{
		DisplayName: req.DisplayName,
		Description: req.Description,
		Permissions: req.Permissions,
		BindableAt:  req.BindableAt,
	})
	if err != nil {
		return mapErr(c, err, "failed to update role", "role", roleID)
	}
	d := auditx.Details{"role_id": after.ID}
	d.Changed("display_name", before.DisplayName, after.DisplayName)
	d.Changed("description", before.Description, after.Description)
	if !slices.Equal(before.Permissions, after.Permissions) {
		d["permissions"] = auditx.Change{From: before.Permissions, To: after.Permissions}
	}
	if !slices.Equal(before.BindableAt, after.BindableAt) {
		d["bindable_at"] = auditx.Change{From: before.BindableAt, To: after.BindableAt}
	}
	auditx.Set(c, d)
	return c.JSON(roleResponse(*after))
}

func (h *Handler) DeleteRole(c fiber.Ctx) error {
	roleID := httpx.PathParam(c, "role_id")
	r, err := h.store.DeleteCustomRole(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), roleID)
	if err != nil {
		return mapErr(c, err, "failed to delete role", "role", roleID)
	}
	if r != nil {
		auditx.Set(c, auditx.Details{"role_id": r.ID, "display_name": auditx.Change{From: r.DisplayName, To: nil}})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) ListPermissions(c fiber.Ctx) error {
	snap := h.src.Current()
	perms, err := h.store.ListPermissions(c.Context(), snap, httpx.PathParam(c, "node_id"))
	if err != nil {
		return mapErr(c, err, "failed to list permissions", "node", "")
	}
	return c.JSON(fiber.Map{"permissions": perms, "custom_roles": snap.CustomRoles})
}

type PatchSecurityPolicyRequest struct {
	AllowPersonalKeys *bool            `json:"allow_personal_keys"`
	AllowedCIDRs      httpx.PatchField `json:"allowed_cidrs"`
	RequiredIdPClaims httpx.PatchField `json:"required_idp_claims"`
}

func (h *Handler) GetSecurityPolicy(c fiber.Ctx) error {
	pol, err := h.store.GetSecurityPolicy(c.Context(), httpx.PathParam(c, "node_id"))
	if err != nil {
		return mapErr(c, err, "failed to get security policy", "node", "")
	}
	return c.JSON(securityPolicyResponse(pol))
}

func (h *Handler) PatchSecurityPolicy(c fiber.Ctx) error {
	var req PatchSecurityPolicyRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	var patch SecurityPolicyPatch
	patch.AllowPersonalKeys = req.AllowPersonalKeys
	if req.AllowedCIDRs.Set {
		patch.AllowedCIDRsSet = true
		cidrs, err := httpx.DecodePatch[[]string](req.AllowedCIDRs, "allowed_cidrs")
		if err != nil {
			return err
		}
		if cidrs != nil {
			patch.AllowedCIDRs = *cidrs
		}
	}
	if req.RequiredIdPClaims.Set {
		patch.RequiredIdPClaimsSet = true
		claims, err := httpx.DecodePatch[map[string][]string](req.RequiredIdPClaims, "required_idp_claims")
		if err != nil {
			return err
		}
		if claims != nil {
			patch.RequiredIdPClaims = *claims
		}
	}
	before, after, err := h.store.PatchSecurityPolicy(c.Context(), httpx.PathParam(c, "node_id"), patch)
	if err != nil {
		return mapErr(c, err, "failed to update security policy", "node", "")
	}
	d := auditx.Details{}
	if before.AllowPersonalKeys != after.AllowPersonalKeys {
		d["allow_personal_keys"] = auditx.Change{From: before.AllowPersonalKeys, To: after.AllowPersonalKeys}
	}
	if !slices.Equal(before.AllowedCIDRs, after.AllowedCIDRs) {
		d["allowed_cidrs"] = auditx.Change{From: before.AllowedCIDRs, To: after.AllowedCIDRs}
	}
	if !claimMapsEqual(before.RequiredIdPClaims, after.RequiredIdPClaims) {
		d["required_idp_claims"] = auditx.Change{From: before.RequiredIdPClaims, To: after.RequiredIdPClaims}
	}
	auditx.Set(c, d)
	return c.JSON(securityPolicyResponse(after))
}
