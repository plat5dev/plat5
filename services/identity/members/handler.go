package members

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/nodes"
)

// MemberHeader is the caller's member, sent by the front door
// (docs/reference/headers.md).
const MemberHeader = "X-Plat5-Member-Id"

// Handler serves the identity API for members and what they hold
// (docs/reference/api/identity.md), and the node routes that need a member:
// creating nodes and the filtered children list.
type Handler struct {
	store *Store
	src   *feed.Source
}

func NewHandler(store *Store, src *feed.Source) *Handler {
	return &Handler{store: store, src: src}
}

func (h *Handler) Mount(r fiber.Router) {
	r.Get("/nodes/:node_id/children/:kind", h.ListChildren)
	r.Post("/nodes/:node_id/children/:kind", h.CreateChild)
	r.Post("/roots/:kind", h.CreateRoot)
	r.Get("/nodes/:node_id/roles", h.Roles)

	r.Get("/nodes/:node_id/bindings", h.ListBindings)
	r.Post("/nodes/:node_id/bindings", h.CreateBinding)
	r.Patch("/nodes/:node_id/bindings/:binding_id", h.PatchBinding)
	r.Delete("/nodes/:node_id/bindings/:binding_id", h.DeleteBinding)
	r.Get("/nodes/:node_id/access", h.Access)

	r.Get("/nodes/:node_id/members", h.ListMembers)
	r.Post("/nodes/:node_id/members", h.AddMember)
	r.Get("/nodes/:node_id/members/:member_id", h.GetMember)
	r.Delete("/nodes/:node_id/members/:member_id", h.RemoveMember)

	r.Get("/members/:member_id", h.Person)
	r.Delete("/members/:member_id", h.DeleteAccount)
	r.Patch("/members/:member_id/status", h.SetStatus)
	r.Get("/members/:member_id/nodes", h.PersonNodes)
	r.Get("/members/:member_id/keys", h.ListKeys)
	r.Post("/members/:member_id/keys", h.CreateKey)
	r.Delete("/members/:member_id/keys/:key_id", h.RevokeKey)
	r.Post("/members/:member_id/invites/redeem", h.Redeem)

	r.Get("/nodes/:node_id/service-accounts", h.ListServiceAccounts)
	r.Post("/nodes/:node_id/service-accounts", h.CreateServiceAccount)
	r.Get("/nodes/:node_id/service-accounts/:member_id", h.GetServiceAccount)
	r.Patch("/nodes/:node_id/service-accounts/:member_id", h.RenameServiceAccount)
	r.Delete("/nodes/:node_id/service-accounts/:member_id", h.DeleteServiceAccount)
	r.Get("/nodes/:node_id/service-accounts/:member_id/keys", h.ListKeys)
	r.Post("/nodes/:node_id/service-accounts/:member_id/keys", h.CreateKey)
	r.Delete("/nodes/:node_id/service-accounts/:member_id/keys/:key_id", h.RevokeKey)

	r.Get("/nodes/:node_id/invites", h.ListInvites)
	r.Post("/nodes/:node_id/invites", h.CreateInvite)
	r.Delete("/nodes/:node_id/invites/:invite_id", h.RevokeInvite)
}

// --- shapes (docs/reference/api/identity.md#shapes) ---

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

type PersonNodeResponse struct {
	NodeResponse
	RootSlug  *string `json:"root_slug"`
	RootLabel string  `json:"root_label"`
}

type BindingResponse struct {
	ID        string  `json:"id"`
	Member    string  `json:"member"`
	NodeID    string  `json:"node_id"`
	Role      string  `json:"role"`
	ExpiresAt *string `json:"expires_at"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

type AccessResponse struct {
	ID        string  `json:"id"`
	Member    string  `json:"member"`
	Principal string  `json:"principal"`
	Name      *string `json:"name"`
	Role      string  `json:"role"`
	NodeID    string  `json:"node_id"`
	Inherited bool    `json:"inherited"`
	ExpiresAt *string `json:"expires_at"`
	Guest     bool    `json:"guest"`
}

type MemberResponse struct {
	ID        string  `json:"id"`
	Principal string  `json:"principal"`
	Name      *string `json:"name"`
	Status    string  `json:"status"`
	Guest     bool    `json:"guest"`
}

type PersonResponse struct {
	ID     string  `json:"id"`
	Name   *string `json:"name"`
	Status string  `json:"status"`
}

type ServiceAccountResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	NodeID    string `json:"node_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type KeyResponse struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	KeyPrefix string  `json:"key_prefix"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
	Key       string  `json:"key,omitempty"`
}

type GrantResponse struct {
	Node     string  `json:"node"`
	Role     string  `json:"role"`
	Duration *string `json:"duration"`
}

type InviteResponse struct {
	ID           string          `json:"id"`
	NodeID       string          `json:"node_id"`
	Role         string          `json:"role"`
	RoleDuration *string         `json:"role_duration"`
	Grants       []GrantResponse `json:"grants"`
	MaxUses      *int            `json:"max_uses"`
	UseCount     int             `json:"use_count"`
	Status       string          `json:"status"`
	ExpiresAt    string          `json:"expires_at"`
	TokenPrefix  string          `json:"token_prefix"`
	CreatedAt    string          `json:"created_at"`
	Token        string          `json:"token,omitempty"`
}

type RoleResponse struct {
	Slug        string   `json:"slug"`
	Labels      []string `json:"labels"`
	MaxDuration *string  `json:"max_duration"`
}

type RolesResponse struct {
	Roles       []RoleResponse `json:"roles"`
	DefaultRole *string        `json:"default_role"`
	CreatorRole *string        `json:"creator_role"`
}

type RedeemResponse struct {
	MemberID string `json:"member_id"`
	RootID   string `json:"root_id"`
	NodeID   string `json:"node_id"`
}

func nodeResponse(n *nodes.Node) NodeResponse {
	return NodeResponse{
		ID: n.ID, Kind: n.Kind, ParentID: n.ParentID, RootID: n.RootID, Slug: n.Slug, Label: n.Label,
		Status: n.Status, DeletedAt: httpx.FormatTimePtr(n.DeletedAt),
		CreatedAt: httpx.FormatTime(n.CreatedAt), UpdatedAt: httpx.FormatTime(n.UpdatedAt),
	}
}

func bindingResponse(b *Binding) BindingResponse {
	return BindingResponse{
		ID: b.ID, Member: b.MemberID, NodeID: b.NodeID, Role: b.Role, ExpiresAt: httpx.FormatTimePtr(b.ExpiresAt),
		CreatedAt: httpx.FormatTime(b.CreatedAt), UpdatedAt: httpx.FormatTime(b.UpdatedAt),
	}
}

func memberResponse(m *Member) MemberResponse {
	return MemberResponse{ID: m.ID, Principal: m.Principal, Name: m.Name, Status: m.Status(), Guest: m.Guest}
}

func serviceAccountResponse(sa *ServiceAccount) ServiceAccountResponse {
	return ServiceAccountResponse{ID: sa.ID, Name: sa.Name, NodeID: sa.NodeID,
		CreatedAt: httpx.FormatTime(sa.CreatedAt), UpdatedAt: httpx.FormatTime(sa.UpdatedAt)}
}

func keyResponse(k *Key) KeyResponse {
	return KeyResponse{ID: k.ID, Name: k.Name, KeyPrefix: k.KeyPrefix, CreatedAt: httpx.FormatTime(k.CreatedAt),
		RevokedAt: httpx.FormatTimePtr(k.RevokedAt), Key: k.Secret}
}

func inviteResponse(inv *Invite) InviteResponse {
	grants := make([]GrantResponse, 0, len(inv.Grants))
	for _, g := range inv.Grants {
		grants = append(grants, GrantResponse{Node: g.NodeID, Role: g.Role, Duration: g.Duration})
	}
	return InviteResponse{
		ID: inv.ID, NodeID: inv.NodeID, Role: inv.Role, RoleDuration: inv.RoleDuration, Grants: grants,
		MaxUses: inv.MaxUses, UseCount: inv.UseCount, Status: inv.Status, ExpiresAt: httpx.FormatTime(inv.ExpiresAt),
		TokenPrefix: inv.TokenPrefix, CreatedAt: httpx.FormatTime(inv.CreatedAt), Token: inv.Token,
	}
}

// --- errors ---

// mapErr maps the store's sentinels: anything not found under the address is
// 404, and a caller outside the tree is a malformed request.
func mapErr(c fiber.Ctx, err error, msg, resource, resourceID string) error {
	if err == ErrNotMember {
		return errors.InvalidRequestError()
	}
	if err == nodes.ErrNotFound {
		return errors.NotFoundError("node", httpx.PathParam(c, "node_id"))
	}
	return httpx.MapDB(c.Context(), err, msg, httpx.DBErr{NotFound: ErrNotFound, Resource: resource, ResourceID: resourceID})
}

// caller is X-Plat5-Member-Id, or "" when none was sent.
func caller(c fiber.Ctx) string {
	return strings.TrimSpace(strings.Clone(c.Get(MemberHeader)))
}

// listState reads ?state=: empty for live nodes, deleted for deleted ones.
func listState(c fiber.Ctx) (bool, error) {
	switch c.Query("state") {
	case "", nodes.StatusActive:
		return false, nil
	case nodes.StatusDeleted:
		return true, nil
	}
	return false, errors.FieldError("state", errors.FallbackValidation)
}

// --- nodes that need a member ---

type CreateNodeRequest struct {
	Label string  `json:"label"`
	Slug  *string `json:"slug"`
}

func (h *Handler) CreateChild(c fiber.Ctx) error {
	var req CreateNodeRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	in := nodes.CreateInput{Kind: httpx.PathParam(c, "kind"), Label: req.Label, Slug: req.Slug}
	n, b, err := h.store.CreateChild(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), in, caller(c))
	if err != nil {
		return mapErr(c, err, "failed to create node", "node", "")
	}
	d := auditx.Details{"node_id": n.ID}
	if b != nil {
		d["binding_id"] = b.ID
	}
	auditx.Set(c, d)
	auditx.SetCreated(c, n.ID)
	return c.Status(fiber.StatusCreated).JSON(nodeResponse(n))
}

func (h *Handler) CreateRoot(c fiber.Ctx) error {
	var req CreateNodeRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	in := nodes.CreateInput{Kind: httpx.PathParam(c, "kind"), Label: req.Label, Slug: req.Slug}
	n, m, err := h.store.CreateRoot(c.Context(), h.src.Current(), in, caller(c))
	if err != nil {
		return mapErr(c, err, "failed to create root", "node", "")
	}
	d := auditx.Details{"node_id": n.ID}
	if m != nil {
		d["member_id"] = m.ID
		auditx.SetRoot(c, n.ID, m.ID)
	}
	auditx.Set(c, d)
	auditx.SetCreated(c, n.ID)
	return c.Status(fiber.StatusCreated).JSON(nodeResponse(n))
}

func (h *Handler) ListChildren(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c, id.Node)
	if err != nil {
		return err
	}
	deleted, err := listState(c)
	if err != nil {
		return err
	}
	callerID := caller(c)
	if callerID == "" {
		return errors.InvalidRequestError()
	}
	list, hasMore, err := h.store.ListChildren(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"),
		httpx.PathParam(c, "kind"), callerID, deleted, limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list nodes", "node", "")
	}
	out := make([]NodeResponse, 0, len(list))
	for _, n := range list {
		out = append(out, nodeResponse(n.Node))
	}
	return c.JSON(fiber.Map{"nodes": out, "has_more": hasMore})
}

func (h *Handler) Roles(c fiber.Ctx) error {
	snap := h.src.Current()
	r, err := h.store.Roles(c.Context(), snap, httpx.PathParam(c, "node_id"))
	if err != nil {
		return mapErr(c, err, "failed to list roles", "node", "")
	}
	out := RolesResponse{Roles: make([]RoleResponse, 0, len(r.Roles)), DefaultRole: r.DefaultRole, CreatorRole: r.CreatorRole}
	for _, role := range r.Roles {
		rr := RoleResponse{Slug: role.Slug, Labels: role.Labels}
		if rr.Labels == nil {
			rr.Labels = []string{}
		}
		if role.MaxDurationText != "" {
			text := role.MaxDurationText
			rr.MaxDuration = &text
		}
		out.Roles = append(out.Roles, rr)
	}
	return c.JSON(out)
}

// --- bindings and access ---

type CreateBindingRequest struct {
	Member    string     `json:"member"`
	Role      string     `json:"role"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type PatchBindingRequest struct {
	Role      *string            `json:"role"`
	ExpiresAt httpx.NullableTime `json:"expires_at"`
}

func (h *Handler) ListBindings(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c, id.Binding)
	if err != nil {
		return err
	}
	list, hasMore, err := h.store.ListBindings(c.Context(), httpx.PathParam(c, "node_id"), limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list bindings", "binding", "")
	}
	out := make([]BindingResponse, 0, len(list))
	for _, b := range list {
		out = append(out, bindingResponse(b))
	}
	return c.JSON(fiber.Map{"bindings": out, "has_more": hasMore})
}

func (h *Handler) CreateBinding(c fiber.Ctx) error {
	var req CreateBindingRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	b, err := h.store.CreateBinding(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"),
		strings.TrimSpace(req.Member), req.Role, req.ExpiresAt)
	if err != nil {
		return mapErr(c, err, "failed to create binding", "binding", "")
	}
	auditx.Set(c, auditx.Details{"binding_id": b.ID})
	auditx.SetCreated(c, b.ID)
	return c.Status(fiber.StatusCreated).JSON(bindingResponse(b))
}

func (h *Handler) PatchBinding(c fiber.Ctx) error {
	var req PatchBindingRequest
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	bindingID := httpx.PathParam(c, "binding_id")
	before, after, err := h.store.PatchBinding(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), bindingID, req.Role, req.ExpiresAt)
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
	auditx.Set(c, auditx.Details{"binding_id": b.ID, "member_id": b.MemberID, "role": auditx.Change{From: b.Role, To: nil}})
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) Access(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c, id.Binding)
	if err != nil {
		return err
	}
	list, hasMore, err := h.store.Access(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list access", "node", "")
	}
	out := make([]AccessResponse, 0, len(list))
	for _, e := range list {
		out = append(out, AccessResponse{
			ID: e.BindingID, Member: e.MemberID, Principal: e.Principal, Name: e.Name, Role: e.Role,
			NodeID: e.NodeID, Inherited: e.Inherited, ExpiresAt: httpx.FormatTimePtr(e.ExpiresAt), Guest: e.Guest,
		})
	}
	return c.JSON(fiber.Map{"access": out, "has_more": hasMore})
}

// --- members ---

type AddMemberRequest struct {
	IdP *struct {
		Issuer  string `json:"issuer"`
		Subject string `json:"subject"`
	} `json:"idp"`
	Role      *string    `json:"role"`
	ExpiresAt *time.Time `json:"expires_at"`
}

func (h *Handler) ListMembers(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c, id.Member)
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
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	in := AddMemberInput{Role: req.Role, ExpiresAt: req.ExpiresAt}
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

func personResponse(m *Member) PersonResponse {
	return PersonResponse{ID: m.ID, Name: m.Name, Status: PersonStatus(m)}
}

func (h *Handler) Person(c fiber.Ctx) error {
	memberID := httpx.PathParam(c, "member_id")
	m, err := h.store.Person(c.Context(), memberID)
	if err != nil {
		return mapErr(c, err, "failed to get person", "member", memberID)
	}
	return c.JSON(personResponse(m))
}

func (h *Handler) DeleteAccount(c fiber.Ctx) error {
	memberID := httpx.PathParam(c, "member_id")
	changed, err := h.store.DeleteAccount(c.Context(), memberID)
	if err != nil {
		return mapErr(c, err, "failed to delete account", "member", memberID)
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
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	memberID := httpx.PathParam(c, "member_id")
	before, m, err := h.store.SetStatus(c.Context(), memberID, req.Status)
	if err != nil {
		return mapErr(c, err, "failed to set status", "member", memberID)
	}
	d := auditx.Details{}
	d.Changed("status", before, req.Status)
	auditx.Set(c, d)
	return c.JSON(personResponse(m))
}

func (h *Handler) PersonNodes(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c, id.Node)
	if err != nil {
		return err
	}
	deleted, err := listState(c)
	if err != nil {
		return err
	}
	memberID := httpx.PathParam(c, "member_id")
	list, hasMore, err := h.store.ListPersonNodes(c.Context(), h.src.Current(), memberID, c.Query("kind"), deleted, limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list nodes", "member", memberID)
	}
	out := make([]PersonNodeResponse, 0, len(list))
	for _, n := range list {
		out = append(out, PersonNodeResponse{NodeResponse: nodeResponse(n.Node), RootSlug: n.RootSlug, RootLabel: n.RootLabel})
	}
	return c.JSON(fiber.Map{"nodes": out, "has_more": hasMore})
}

// --- service accounts ---

type CreateServiceAccountRequest struct {
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	ExpiresAt *time.Time `json:"expires_at"`
}

func (h *Handler) ListServiceAccounts(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c, id.Member)
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
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	sa, b, err := h.store.CreateServiceAccount(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), req.Name, req.Role, req.ExpiresAt)
	if err != nil {
		return mapErr(c, err, "failed to create service account", "service_account", "")
	}
	auditx.Set(c, auditx.Details{"member_id": sa.ID, "binding_id": b.ID})
	auditx.SetCreated(c, sa.ID)
	return c.Status(fiber.StatusCreated).JSON(serviceAccountResponse(sa))
}

func (h *Handler) GetServiceAccount(c fiber.Ctx) error {
	memberID := httpx.PathParam(c, "member_id")
	sa, err := h.store.GetServiceAccount(c.Context(), httpx.PathParam(c, "node_id"), memberID)
	if err != nil {
		return mapErr(c, err, "failed to get service account", "service_account", memberID)
	}
	return c.JSON(serviceAccountResponse(sa))
}

func (h *Handler) RenameServiceAccount(c fiber.Ctx) error {
	var req struct {
		Name *string `json:"name"`
	}
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	if req.Name == nil {
		return errors.FieldError("name", "Nothing to update.")
	}
	memberID := httpx.PathParam(c, "member_id")
	before, sa, err := h.store.RenameServiceAccount(c.Context(), httpx.PathParam(c, "node_id"), memberID, *req.Name)
	if err != nil {
		return mapErr(c, err, "failed to rename service account", "service_account", memberID)
	}
	d := auditx.Details{}
	d.Changed("name", before, sa.Name)
	auditx.Set(c, d)
	return c.JSON(serviceAccountResponse(sa))
}

func (h *Handler) DeleteServiceAccount(c fiber.Ctx) error {
	memberID := httpx.PathParam(c, "member_id")
	if err := h.store.DeleteServiceAccount(c.Context(), httpx.PathParam(c, "node_id"), memberID); err != nil {
		return mapErr(c, err, "failed to delete service account", "service_account", memberID)
	}
	d := auditx.Details{"member_id": memberID}
	d.Changed("status", StatusActive, StatusRemoved)
	auditx.Set(c, d)
	return c.SendStatus(fiber.StatusNoContent)
}

// --- keys, on a service account or a person ---

func keyOwner(c fiber.Ctx) KeyOwner {
	return KeyOwner{NodeID: httpx.PathParam(c, "node_id"), MemberID: httpx.PathParam(c, "member_id")}
}

func (h *Handler) ListKeys(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c, id.Key)
	if err != nil {
		return err
	}
	owner := keyOwner(c)
	list, hasMore, err := h.store.ListKeys(c.Context(), owner, limit, after)
	if err != nil {
		return mapErr(c, err, "failed to list keys", "member", owner.MemberID)
	}
	out := make([]KeyResponse, 0, len(list))
	for _, k := range list {
		out = append(out, keyResponse(k))
	}
	return c.JSON(fiber.Map{"keys": out, "has_more": hasMore})
}

func (h *Handler) CreateKey(c fiber.Ctx) error {
	var req struct {
		Name string `json:"name"`
	}
	if len(c.Body()) > 0 {
		if err := c.Bind().Body(&req); err != nil {
			return err
		}
	}
	owner := keyOwner(c)
	k, err := h.store.CreateKey(c.Context(), owner, req.Name)
	if err != nil {
		return mapErr(c, err, "failed to create key", "member", owner.MemberID)
	}
	auditx.Set(c, auditx.Details{"key_id": k.ID, "key_prefix": k.KeyPrefix})
	auditx.SetCreated(c, k.ID)
	return c.Status(fiber.StatusCreated).JSON(keyResponse(k))
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

// --- invites ---

type CreateInviteRequest struct {
	Role         *string           `json:"role"`
	RoleDuration *string           `json:"role_duration"`
	Grants       []GrantInput      `json:"grants"`
	MaxUses      httpx.NullableInt `json:"max_uses"`
	ExpiresAt    *time.Time        `json:"expires_at"`
}

func (h *Handler) ListInvites(c fiber.Ctx) error {
	limit, after, err := httpx.ParseListParams(c, id.Invite)
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
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	in := InviteInput{Role: req.Role, RoleDuration: req.RoleDuration, Grants: req.Grants, ExpiresAt: req.ExpiresAt}
	if req.MaxUses.Set {
		in.MaxUses = req.MaxUses.Value
	} else {
		one := 1
		in.MaxUses = &one
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
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	memberID := httpx.PathParam(c, "member_id")
	r, err := h.store.Redeem(c.Context(), memberID, strings.TrimSpace(req.Token))
	if err != nil {
		return mapErr(c, err, "failed to redeem invite", "member", memberID)
	}
	d := auditx.Details{"node_id": r.NodeID}
	if r.MemberCreated {
		d["member_id"] = r.MemberID
	}
	if r.Binding != nil {
		d["binding_id"] = r.Binding.ID
		auditx.SetCreated(c, r.Binding.ID)
	}
	auditx.Set(c, d)
	auditx.SetRoot(c, r.RootID, r.MemberID)
	return c.JSON(RedeemResponse{MemberID: r.MemberID, RootID: r.RootID, NodeID: r.NodeID})
}
