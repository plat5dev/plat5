package members

import (
	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/nodes"
)

type CreateNodeRequest struct {
	DisplayName string  `json:"display_name"`
	Slug        *string `json:"slug"`
}

func (h *Handler) CreateChild(c fiber.Ctx) error {
	var req CreateNodeRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	in := nodes.CreateInput{Kind: httpx.PathParam(c, "kind"), DisplayName: req.DisplayName, Slug: req.Slug}
	n, b, err := h.store.CreateChild(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), in, header(c, MemberHeader))
	if err != nil {
		return mapErr(c, err, "failed to create node", "node", "")
	}
	d := auditx.Details{"node_id": n.ID}
	if b != nil {
		d["binding_id"] = b.ID
	}
	auditx.Set(c, d)
	auditx.SetCreated(c, n.ID)
	return h.store.nodes.Respond(c, fiber.StatusCreated, n)
}

func (h *Handler) CreateRoot(c fiber.Ctx) error {
	var req CreateNodeRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	in := nodes.CreateInput{Kind: httpx.PathParam(c, "kind"), DisplayName: req.DisplayName, Slug: req.Slug}
	n, memberID, err := h.store.CreateRoot(c.Context(), h.src.Current(), in, header(c, UserHeader))
	if err != nil {
		return mapErr(c, err, "failed to create root", "node", "")
	}
	d := auditx.Details{"node_id": n.ID}
	if memberID != "" {
		d["member_id"] = memberID
		auditx.SetNamed(c, []string{n.ID}, memberID)
	}
	auditx.Set(c, d)
	auditx.SetCreated(c, n.ID)
	return h.store.nodes.Respond(c, fiber.StatusCreated, n)
}

func (h *Handler) ListChildren(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}
	deleted, err := listStatus(c)
	if err != nil {
		return err
	}
	list, hasMore, err := h.store.ListChildren(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"),
		httpx.PathParam(c, "kind"), header(c, MemberHeader), deleted, limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list nodes", "node", "")
	}
	return h.store.nodes.RespondList(c, list, hasMore)
}
