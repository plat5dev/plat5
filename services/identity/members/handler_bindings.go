package members

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

type CreateBindingRequest struct {
	MemberID        string           `json:"member_id"`
	GroupID         string           `json:"group_id"`
	PublicPrincipal string           `json:"public_principal"`
	Role            string           `json:"role"`
	ExpiresAt       httpx.PatchField `json:"expires_at"`
	ExpiresIn       httpx.PatchField `json:"expires_in"`
}

type PatchBindingRequest struct {
	Role      httpx.PatchField `json:"role"`
	ExpiresAt httpx.PatchField `json:"expires_at"`
	ExpiresIn httpx.PatchField `json:"expires_in"`
}

func (h *Handler) ListBindings(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}
	inherited := true
	switch c.Query("inherited") {
	case "", "true":
	case "false":
		inherited = false
	default:
		return errors.FieldError("inherited", errors.FallbackValidation)
	}
	list, hasMore, err := h.store.ListBindings(c.Context(), httpx.PathParam(c, "node_id"), inherited, limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list bindings", "binding", "")
	}
	return c.JSON(fiber.Map{"bindings": bindingResponses(list), "has_more": hasMore})
}

func (h *Handler) CreateBinding(c fiber.Ctx) error {
	var req CreateBindingRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	memberID := strings.TrimSpace(req.MemberID)
	groupID := strings.TrimSpace(req.GroupID)
	publicPrincipal := strings.TrimSpace(req.PublicPrincipal)
	subjects := 0
	if memberID != "" {
		subjects++
	}
	if groupID != "" {
		subjects++
	}
	if publicPrincipal != "" {
		subjects++
	}
	if subjects != 1 {
		if subjects == 0 {
			return errors.FieldError("member_id", "Choose a member, group, or public_principal.")
		}
		return errors.FieldError("group_id", "Send only one of member_id, group_id, or public_principal.")
	}
	_, expiresAt, err := parseExpiry(req.ExpiresAt, req.ExpiresIn, h.store.now().UTC())
	if err != nil {
		return err
	}
	var b *Binding
	switch {
	case groupID != "":
		b, err = h.store.CreateGroupBinding(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), groupID, req.Role, expiresAt)
	case publicPrincipal != "":
		b, err = h.store.CreatePublicBinding(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), publicPrincipal, req.Role, expiresAt)
	default:
		b, err = h.store.CreateBinding(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), memberID, req.Role, expiresAt)
	}
	if err != nil {
		return mapErr(c, err, "failed to create binding", "binding", "")
	}
	auditx.Set(c, auditx.Details{"binding_id": b.ID})
	auditx.SetCreated(c, b.ID)
	return c.Status(fiber.StatusCreated).JSON(bindingResponse(b))
}

func (h *Handler) PatchBinding(c fiber.Ctx) error {
	var req PatchBindingRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	var patch BindingPatch
	if req.Role.Set {
		role, err := httpx.DecodeRequiredPatch[string](req.Role, "role")
		if err != nil {
			return err
		}
		patch.Role = &role
	}
	expSet, expVal, err := parseExpiry(req.ExpiresAt, req.ExpiresIn, h.store.now().UTC())
	if err != nil {
		return err
	}
	patch.ExpiresAtSet = expSet
	patch.ExpiresAt = expVal
	bindingID := httpx.PathParam(c, "binding_id")
	before, after, err := h.store.PatchBinding(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), bindingID, patch)
	if err != nil {
		return mapErr(c, err, "failed to update binding", "binding", bindingID)
	}
	d := auditx.Details{}
	d.Changed("role", before.Role, after.Role)
	d.ChangedPtr("expires_at", httpx.FormatTimePtr(before.ExpiresAt), httpx.FormatTimePtr(after.ExpiresAt))
	auditx.Set(c, d)
	return c.JSON(bindingResponse(after))
}

func (h *Handler) DeleteBinding(c fiber.Ctx) error {
	bindingID := httpx.PathParam(c, "binding_id")
	b, err := h.store.DeleteBinding(c.Context(), httpx.PathParam(c, "node_id"), bindingID)
	if err != nil {
		return mapErr(c, err, "failed to delete binding", "binding", bindingID)
	}
	if b != nil {
		d := auditx.Details{"binding_id": b.ID, "role": auditx.Change{From: b.Role, To: nil}}
		if b.MemberID != nil {
			d["member_id"] = *b.MemberID
		} else if b.GroupID != nil {
			d["group_id"] = *b.GroupID
		} else if b.PublicPrincipal != nil {
			d["public_principal"] = *b.PublicPrincipal
		}
		auditx.Set(c, d)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) Membership(c fiber.Ctx) error {
	memberID := httpx.PathParam(c, "member_id")
	m, err := h.store.Membership(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), memberID)
	if err != nil {
		return mapErr(c, err, "failed to read membership", "member", memberID)
	}
	return c.JSON(MembershipResponse{MemberID: m.MemberID, Bindings: bindingResponses(m.Bindings), Permissions: m.Permissions})
}
