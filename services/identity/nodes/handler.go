package nodes

import (
	"context"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

// Handler serves the node routes that need no member
// (docs/reference/api/identity.md#nodes). Creating nodes and the children
// list are in package members.
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

// NodeResponse is the one node shape (docs/reference/api/identity.md#node).
type NodeResponse struct {
	ID          string             `json:"id"`
	Kind        string             `json:"kind"`
	ParentID    *string            `json:"parent_id"`
	RootID      string             `json:"root_id"`
	Slug        *string            `json:"slug"`
	DisplayName string             `json:"display_name"`
	Status      string             `json:"status"`
	DeletedAt   *string            `json:"deleted_at"`
	PurgeAfter  *string            `json:"purge_after"`
	CreatedAt   string             `json:"created_at"`
	UpdatedAt   string             `json:"updated_at"`
	Ancestors   []AncestorResponse `json:"ancestors"`
}

type AncestorResponse struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Slug        *string `json:"slug"`
	DisplayName string  `json:"display_name"`
}

// Present is the response shape of ns, with their ancestors.
func (s *Store) Present(ctx context.Context, ns ...*Node) ([]NodeResponse, error) {
	ids := make([]string, len(ns))
	for i, n := range ns {
		ids[i] = n.ID
	}
	ancestors, err := s.Ancestors(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]NodeResponse, 0, len(ns))
	for _, n := range ns {
		r := NodeResponse{
			ID:          n.ID,
			Kind:        n.Kind,
			ParentID:    n.ParentID,
			RootID:      n.RootID,
			Slug:        n.Slug,
			DisplayName: n.DisplayName,
			Status:      n.Status,
			DeletedAt:   httpx.FormatTimePtr(n.DeletedAt),
			PurgeAfter:  httpx.FormatTimePtr(n.PurgeAfter),
			CreatedAt:   httpx.FormatTime(n.CreatedAt),
			UpdatedAt:   httpx.FormatTime(n.UpdatedAt),
			Ancestors:   []AncestorResponse{},
		}
		for _, a := range ancestors[n.ID] {
			r.Ancestors = append(r.Ancestors, AncestorResponse{ID: a.ID, Kind: a.Kind, Slug: a.Slug, DisplayName: a.DisplayName})
		}
		out = append(out, r)
	}
	return out, nil
}

// Respond sends n with status.
func (s *Store) Respond(c fiber.Ctx, status int, n *Node) error {
	out, err := s.Present(c.Context(), n)
	if err != nil {
		return httpx.MapDB(c.Context(), err, "failed to read node", httpx.DBErr{})
	}
	return c.Status(status).JSON(out[0])
}

// RespondList sends ns under nodes, with has_more.
func (s *Store) RespondList(c fiber.Ctx, ns []*Node, hasMore bool) error {
	out, err := s.Present(c.Context(), ns...)
	if err != nil {
		return httpx.MapDB(c.Context(), err, "failed to read nodes", httpx.DBErr{})
	}
	return c.JSON(fiber.Map{"nodes": out, "has_more": hasMore})
}

type UpdateRequest struct {
	DisplayName *string `json:"display_name"`
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
	return h.store.Respond(c, fiber.StatusOK, n)
}

func (h *Handler) List(c fiber.Ctx) error {
	ctx := c.Context()
	limit, startingAfter, err := httpx.ParseListParams(c)
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
	return h.store.RespondList(c, list, hasMore)
}

func (h *Handler) Update(c fiber.Ctx) error {
	ctx := c.Context()
	nodeID := httpx.PathParam(c, "node_id")
	var req UpdateRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	if req.DisplayName == nil {
		return errors.FieldError("display_name", "Display name is required.")
	}
	before, n, err := h.store.UpdateDisplayName(ctx, nodeID, *req.DisplayName)
	if err != nil {
		return httpx.MapDB(ctx, err, "failed to update node", notFound(nodeID))
	}
	d := auditx.Details{}
	d.Changed("display_name", before, n.DisplayName)
	auditx.Set(c, d)
	return h.store.Respond(c, fiber.StatusOK, n)
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
	return h.store.Respond(c, fiber.StatusOK, n)
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
	return h.store.Respond(c, fiber.StatusOK, n)
}
