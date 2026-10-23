package nodes

import (
	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/internal/id"
)

// Handler serves the node routes that need no member
// (docs/reference/api/identity.md#nodes). Creating nodes and the filtered
// children list come with members.
type Handler struct {
	store *Store
	src   *feed.Source
}

func NewHandler(store *Store, src *feed.Source) *Handler {
	return &Handler{store: store, src: src}
}

func (h *Handler) Mount(r fiber.Router) {
	r.Get("/nodes", h.List)
	r.Get("/nodes/:node_id", h.Get)
	r.Patch("/nodes/:node_id", h.Update)
	r.Delete("/nodes/:node_id", h.Delete)
	r.Post("/nodes/:node_id/restore", h.Restore)
	r.Post("/nodes/:node_id/release-slug", h.ReleaseSlug)
}

type NodeResponse struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	ParentID  *string `json:"parent_id"`
	RootID    string  `json:"root_id"`
	Slug      *string `json:"slug"`
	Label     string  `json:"label"`
	Status    string  `json:"status"`
	DeletedAt *string `json:"deleted_at"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

type ListResponse struct {
	Nodes   []NodeResponse `json:"nodes"`
	HasMore bool           `json:"has_more"`
}

type UpdateRequest struct {
	Label *string `json:"label"`
}

func toResponse(n *Node) NodeResponse {
	return NodeResponse{
		ID:        n.ID,
		Kind:      n.Kind,
		ParentID:  n.ParentID,
		RootID:    n.RootID,
		Slug:      n.Slug,
		Label:     n.Label,
		Status:    n.Status,
		DeletedAt: httpx.FormatTimePtr(n.DeletedAt),
		CreatedAt: httpx.FormatTime(n.CreatedAt),
		UpdatedAt: httpx.FormatTime(n.UpdatedAt),
	}
}

func notFound(nodeID string) httpx.DBErr {
	return httpx.DBErr{NotFound: ErrNotFound, Resource: "node", ResourceID: nodeID}
}

func (h *Handler) Get(c fiber.Ctx) error {
	ctx := c.Context()
	nodeID := httpx.PathParam(c, "node_id")
	n, err := h.store.Get(ctx, nodeID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to get node", notFound(nodeID))
	}
	return c.JSON(toResponse(n))
}

func (h *Handler) List(c fiber.Ctx) error {
	ctx := c.Context()
	limit, startingAfter, err := httpx.ParseListParams(c, id.Node)
	if err != nil {
		return err
	}
	f := ListFilter{Kind: c.Query("kind"), ParentID: c.Query("parent_id"), Status: c.Query("status")}
	switch f.Status {
	case "", StatusActive, StatusDeleted, StatusPurged:
	default:
		return errors.FieldError("status", errors.FallbackValidation)
	}
	list, hasMore, err := h.store.List(ctx, f, limit, startingAfter)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to list nodes", httpx.DBErr{})
	}
	out := ListResponse{Nodes: make([]NodeResponse, 0, len(list)), HasMore: hasMore}
	for _, n := range list {
		out.Nodes = append(out.Nodes, toResponse(n))
	}
	return c.JSON(out)
}

func (h *Handler) Update(c fiber.Ctx) error {
	ctx := c.Context()
	nodeID := httpx.PathParam(c, "node_id")
	var req UpdateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	if req.Label == nil {
		return errors.FieldError("label", "Label is required.")
	}
	before, n, err := h.store.UpdateLabel(ctx, nodeID, *req.Label)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to update node", notFound(nodeID))
	}
	d := auditx.Details{}
	d.Changed("label", before, n.Label)
	auditx.Set(c, d)
	return c.JSON(toResponse(n))
}

func (h *Handler) Delete(c fiber.Ctx) error {
	ctx := c.Context()
	nodeID := httpx.PathParam(c, "node_id")
	changed, err := h.store.Delete(ctx, h.src.Current(), nodeID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to delete node", notFound(nodeID))
	}
	if changed {
		d := auditx.Details{}
		d.Changed("status", StatusActive, StatusDeleted)
		auditx.Set(c, d)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) Restore(c fiber.Ctx) error {
	ctx := c.Context()
	nodeID := httpx.PathParam(c, "node_id")
	n, changed, err := h.store.Restore(ctx, h.src.Current(), nodeID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to restore node", notFound(nodeID))
	}
	if changed {
		d := auditx.Details{}
		d.Changed("status", StatusDeleted, StatusActive)
		auditx.Set(c, d)
	}
	return c.JSON(toResponse(n))
}

func (h *Handler) ReleaseSlug(c fiber.Ctx) error {
	ctx := c.Context()
	nodeID := httpx.PathParam(c, "node_id")
	freed, n, err := h.store.ReleaseSlug(ctx, nodeID)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to release slug", notFound(nodeID))
	}
	d := auditx.Details{}
	d.ChangedPtr("slug", freed, n.Slug)
	auditx.Set(c, d)
	return c.JSON(toResponse(n))
}
