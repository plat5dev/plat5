package members

import (
	"encoding/json"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
)

// --- members ---

type AddMemberRequest struct {
	IdP *struct {
		Issuer  string `json:"issuer"`
		Subject string `json:"subject"`
	} `json:"idp"`
	Role string `json:"role"`
}

func (h *Handler) ListMembers(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}
	list, hasMore, err := h.store.ListMembers(c.Context(), httpx.PathParam(c, "node_id"), limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list members", "member", "")
	}
	out := make([]MemberResponse, 0, len(list))
	for _, m := range list {
		out = append(out, memberResponse(m))
	}
	return c.JSON(fiber.Map{"members": out, "has_more": hasMore})
}

func (h *Handler) AddMember(c fiber.Ctx) error {
	var req AddMemberRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	in := AddMemberInput{Role: req.Role}
	if req.IdP != nil {
		in.Issuer, in.Subject = req.IdP.Issuer, req.IdP.Subject
	}
	m, b, err := h.store.AddMember(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), in)
	if err != nil {
		return mapErr(c, err, "failed to add member", "member", "")
	}
	auditx.Set(c, auditx.Details{"member_id": m.ID, "binding_id": b.ID})
	auditx.SetCreated(c, m.ID)
	return c.Status(fiber.StatusCreated).JSON(memberResponse(m))
}

func (h *Handler) GetMember(c fiber.Ctx) error {
	memberID := httpx.PathParam(c, "member_id")
	m, err := h.store.GetMember(c.Context(), httpx.PathParam(c, "node_id"), memberID)
	if err != nil {
		return mapErr(c, err, "failed to get member", "member", memberID)
	}
	return c.JSON(memberResponse(m))
}

func (h *Handler) SetMemberStatus(c fiber.Ctx) error {
	var req struct {
		Status string `json:"status"`
	}
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	memberID := httpx.PathParam(c, "member_id")
	before, m, err := h.store.SetMemberStatus(c.Context(), httpx.PathParam(c, "node_id"), memberID, req.Status)
	if err != nil {
		return mapErr(c, err, "failed to set member status", "member", memberID)
	}
	d := auditx.Details{"member_id": memberID}
	d.Changed("status", before, m.Status())
	auditx.Set(c, d)
	return c.JSON(memberResponse(m))
}

func (h *Handler) RemoveMember(c fiber.Ctx) error {
	memberID := httpx.PathParam(c, "member_id")
	changed, err := h.store.RemoveMember(c.Context(), httpx.PathParam(c, "node_id"), memberID)
	if err != nil {
		return mapErr(c, err, "failed to remove member", "member", memberID)
	}
	if changed {
		d := auditx.Details{"member_id": memberID}
		d.Changed("status", StatusActive, StatusRemoved)
		auditx.Set(c, d)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) MemberUser(c fiber.Ctx) error {
	memberID := httpx.PathParam(c, "member_id")
	u, err := h.store.MemberUser(c.Context(), memberID)
	if err != nil {
		return mapErr(c, err, "failed to get user", "user", "")
	}
	return c.JSON(userResponse(u))
}

// --- groups ---

type GroupNameRequest struct {
	Name string `json:"name"`
}

type AddGroupMemberRequest struct {
	MemberID string `json:"member_id"`
}

func (h *Handler) ListGroups(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}
	list, hasMore, err := h.store.ListGroups(c.Context(), httpx.PathParam(c, "node_id"), limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list groups", "group", "")
	}
	out := make([]GroupResponse, 0, len(list))
	for _, g := range list {
		out = append(out, groupResponse(g))
	}
	return c.JSON(fiber.Map{"groups": out, "has_more": hasMore})
}

func (h *Handler) CreateGroup(c fiber.Ctx) error {
	var req GroupNameRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	g, err := h.store.CreateGroup(c.Context(), httpx.PathParam(c, "node_id"), req.Name)
	if err != nil {
		return mapErr(c, err, "failed to create group", "group", "")
	}
	auditx.Set(c, auditx.Details{"group_id": g.ID})
	auditx.SetCreated(c, g.ID)
	return c.Status(fiber.StatusCreated).JSON(groupResponse(g))
}

func (h *Handler) GetGroup(c fiber.Ctx) error {
	groupID := httpx.PathParam(c, "group_id")
	g, err := h.store.GetGroup(c.Context(), httpx.PathParam(c, "node_id"), groupID)
	if err != nil {
		return mapErr(c, err, "failed to get group", "group", groupID)
	}
	return c.JSON(groupResponse(g))
}

func (h *Handler) RenameGroup(c fiber.Ctx) error {
	var req GroupNameRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	groupID := httpx.PathParam(c, "group_id")
	before, after, err := h.store.RenameGroup(c.Context(), httpx.PathParam(c, "node_id"), groupID, req.Name)
	if err != nil {
		return mapErr(c, err, "failed to rename group", "group", groupID)
	}
	d := auditx.Details{}
	d.Changed("name", before, after.Name)
	auditx.Set(c, d)
	return c.JSON(groupResponse(after))
}

func (h *Handler) DeleteGroup(c fiber.Ctx) error {
	groupID := httpx.PathParam(c, "group_id")
	g, err := h.store.DeleteGroup(c.Context(), httpx.PathParam(c, "node_id"), groupID)
	if err != nil {
		return mapErr(c, err, "failed to delete group", "group", groupID)
	}
	if g != nil {
		auditx.Set(c, auditx.Details{"group_id": g.ID, "name": auditx.Change{From: g.Name, To: nil}})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) ListGroupMembers(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}
	groupID := httpx.PathParam(c, "group_id")
	list, hasMore, err := h.store.ListGroupMembers(c.Context(), httpx.PathParam(c, "node_id"), groupID, limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list group members", "group", groupID)
	}
	out := make([]MemberResponse, 0, len(list))
	for _, m := range list {
		out = append(out, memberResponse(m))
	}
	return c.JSON(fiber.Map{"members": out, "has_more": hasMore})
}

func (h *Handler) AddGroupMember(c fiber.Ctx) error {
	var req AddGroupMemberRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	groupID := httpx.PathParam(c, "group_id")
	memberID := strings.TrimSpace(req.MemberID)
	m, err := h.store.AddGroupMember(c.Context(), httpx.PathParam(c, "node_id"), groupID, memberID)
	if err != nil {
		return mapErr(c, err, "failed to add group member", "group", groupID)
	}
	auditx.Set(c, auditx.Details{"group_id": groupID, "member_id": m.ID})
	return c.Status(fiber.StatusCreated).JSON(memberResponse(m))
}

func (h *Handler) RemoveGroupMember(c fiber.Ctx) error {
	groupID := httpx.PathParam(c, "group_id")
	memberID := httpx.PathParam(c, "member_id")
	changed, err := h.store.RemoveGroupMember(c.Context(), httpx.PathParam(c, "node_id"), groupID, memberID)
	if err != nil {
		return mapErr(c, err, "failed to remove group member", "group", groupID)
	}
	if changed {
		auditx.Set(c, auditx.Details{"group_id": groupID, "member_id": memberID})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// --- invites ---

type CreateInviteRequest struct {
	Role      string           `json:"role"`
	Grants    []Grant          `json:"grants"`
	MaxUses   httpx.PatchField `json:"max_uses"`
	ExpiresIn *string          `json:"expires_in"`
}

// read is the request with its defaults: max_uses omitted is 1, a number is
// that many and null is unlimited; expires_in omitted is 7d.
func (r CreateInviteRequest) read() (InviteInput, error) {
	in := InviteInput{Role: r.Role, Grants: r.Grants, ExpiresIn: DefaultInviteExpiry}
	switch {
	case !r.MaxUses.Set:
		one := 1
		in.MaxUses = &one
	case r.MaxUses.IsNull():
	default:
		var n int
		if err := json.Unmarshal(r.MaxUses.Raw, &n); err != nil || n < 1 {
			return in, MaxUsesError()
		}
		in.MaxUses = &n
	}
	if r.ExpiresIn != nil {
		d, err := feed.ParseDuration(*r.ExpiresIn)
		if err != nil || *r.ExpiresIn == "" {
			return in, ExpiresInError()
		}
		in.ExpiresIn = d
	}
	return in, nil
}

func (h *Handler) ListInvites(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c)
	if err != nil {
		return err
	}
	list, hasMore, err := h.store.ListInvites(c.Context(), httpx.PathParam(c, "node_id"), limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list invites", "invite", "")
	}
	out := make([]InviteResponse, 0, len(list))
	for _, inv := range list {
		out = append(out, inviteResponse(inv))
	}
	return c.JSON(fiber.Map{"invites": out, "has_more": hasMore})
}

func (h *Handler) CreateInvite(c fiber.Ctx) error {
	var req CreateInviteRequest
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	in, err := req.read()
	if err != nil {
		return err
	}
	inv, err := h.store.CreateInvite(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), in)
	if err != nil {
		return mapErr(c, err, "failed to create invite", "invite", "")
	}
	auditx.Set(c, auditx.Details{"invite_id": inv.ID})
	auditx.SetCreated(c, inv.ID)
	return c.Status(fiber.StatusCreated).JSON(inviteResponse(inv))
}

func (h *Handler) RevokeInvite(c fiber.Ctx) error {
	inviteID := httpx.PathParam(c, "invite_id")
	changed, err := h.store.RevokeInvite(c.Context(), httpx.PathParam(c, "node_id"), inviteID)
	if err != nil {
		return mapErr(c, err, "failed to revoke invite", "invite", inviteID)
	}
	if changed {
		d := auditx.Details{"invite_id": inviteID}
		d.Changed("status", InviteActive, InviteRevoked)
		auditx.Set(c, d)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) Redeem(c fiber.Ctx) error {
	var req struct {
		Token string `json:"token"`
	}
	if err := httpx.Body(c, &req); err != nil {
		return err
	}
	userID := httpx.PathParam(c, "user_id")
	r, err := h.store.Redeem(c.Context(), userID, strings.TrimSpace(req.Token))
	if err != nil {
		return mapErr(c, err, "failed to redeem invite", "user", userID)
	}
	presented, err := h.store.nodes.Present(c.Context(), r.Node)
	if err != nil {
		return mapErr(c, err, "failed to read node", "node", "")
	}
	node := presented[0]
	d := auditx.Details{"node_id": node.ID}
	if r.MemberCreated {
		d["member_id"] = r.MemberID
	}
	if r.Binding != nil {
		d["binding_id"] = r.Binding.ID
		auditx.SetCreated(c, r.Binding.ID)
	}
	auditx.Set(c, d)
	chain := make([]string, 0, len(node.Ancestors)+1)
	for _, a := range node.Ancestors {
		chain = append(chain, a.ID)
	}
	auditx.SetNamed(c, append(chain, node.ID), r.MemberID)
	return c.JSON(RedeemResponse{MemberID: r.MemberID, Node: node})
}
