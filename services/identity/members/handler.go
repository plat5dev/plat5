package members

import (
	"encoding/json"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/auditx"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/nodes"
)

// The caller, as the front door sends it (docs/reference/headers.md). Only
// three routes read them (docs/reference/api/identity.md).
const (
	MemberHeader = "X-Plat5-Member-Id"
	UserHeader   = "X-Plat5-User-Id"
)

// Handler serves the identity API for members and what they hold
// (docs/reference/api/identity.md), and the node routes that read the caller:
// creating nodes and the children list.
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
	r.Get("/nodes/:node_id/memberships/:member_id", h.Membership)

	r.Get("/nodes/:node_id/members", h.ListMembers)
	r.Post("/nodes/:node_id/members", h.AddMember)
	r.Get("/nodes/:node_id/members/:member_id", h.GetMember)
	r.Delete("/nodes/:node_id/members/:member_id", h.RemoveMember)
	r.Get("/members/:member_id/user", h.MemberUser)

	r.Get("/users", h.LookupUser)
	r.Get("/users/:user_id", h.GetUser)
	r.Delete("/users/:user_id", h.DeleteAccount)
	r.Patch("/users/:user_id/status", h.SetStatus)
	r.Get("/users/:user_id/nodes", h.UserNodes)
	r.Get("/users/:user_id/keys", h.ListKeys)
	r.Post("/users/:user_id/keys", h.CreateKey)
	r.Delete("/users/:user_id/keys/:key_id", h.RevokeKey)
	r.Post("/users/:user_id/invites/redeem", h.Redeem)

	r.Get("/nodes/:node_id/service-accounts", h.ListServiceAccounts)
	r.Post("/nodes/:node_id/service-accounts", h.CreateServiceAccount)
	r.Get("/nodes/:node_id/service-accounts/:service_account_id", h.GetServiceAccount)
	r.Patch("/nodes/:node_id/service-accounts/:service_account_id", h.RenameServiceAccount)
	r.Delete("/nodes/:node_id/service-accounts/:service_account_id", h.DeleteServiceAccount)
	r.Get("/nodes/:node_id/service-accounts/:service_account_id/keys", h.ListKeys)
	r.Post("/nodes/:node_id/service-accounts/:service_account_id/keys", h.CreateKey)
	r.Delete("/nodes/:node_id/service-accounts/:service_account_id/keys/:key_id", h.RevokeKey)

	r.Get("/nodes/:node_id/invites", h.ListInvites)
	r.Post("/nodes/:node_id/invites", h.CreateInvite)
	r.Delete("/nodes/:node_id/invites/:invite_id", h.RevokeInvite)
}

// --- shapes (docs/reference/api/identity.md#shapes) ---

type BindingResponse struct {
	ID        string  `json:"id"`
	MemberID  string  `json:"member_id"`
	NodeID    string  `json:"node_id"`
	Role      string  `json:"role"`
	Inherited bool    `json:"inherited"`
	Guest     bool    `json:"guest"`
	Principal string  `json:"principal"`
	Name      *string `json:"name"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

type MembershipResponse struct {
	MemberID    string            `json:"member_id"`
	Bindings    []BindingResponse `json:"bindings"`
	Permissions []string          `json:"permissions"`
}

type MemberResponse struct {
	ID        string  `json:"id"`
	Principal string  `json:"principal"`
	Name      *string `json:"name"`
	Status    string  `json:"status"`
	Guest     bool    `json:"guest"`
}

type UserResponse struct {
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

type InviteResponse struct {
	ID          string  `json:"id"`
	NodeID      string  `json:"node_id"`
	Role        string  `json:"role"`
	Grants      []Grant `json:"grants"`
	MaxUses     any     `json:"max_uses"`
	UseCount    int     `json:"use_count"`
	Status      string  `json:"status"`
	ExpiresAt   string  `json:"expires_at"`
	TokenPrefix string  `json:"token_prefix"`
	CreatedAt   string  `json:"created_at"`
	Token       string  `json:"token,omitempty"`
}

type RoleResponse struct {
	Slug        string   `json:"slug"`
	Permissions []string `json:"permissions"`
}

type RolesResponse struct {
	Roles       []RoleResponse `json:"roles"`
	CreatorRole string         `json:"creator_role"`
}

type RedeemResponse struct {
	MemberID string             `json:"member_id"`
	Node     nodes.NodeResponse `json:"node"`
}

// unlimited is max_uses with no cap, on the wire.
const unlimited = "unlimited"

func bindingResponse(b *Binding) BindingResponse {
	return BindingResponse{
		ID: b.ID, MemberID: b.MemberID, NodeID: b.NodeID, Role: b.Role, Inherited: b.Inherited, Guest: b.Guest,
		Principal: b.Principal, Name: b.Name, CreatedAt: httpx.FormatTime(b.CreatedAt), UpdatedAt: httpx.FormatTime(b.UpdatedAt),
	}
}

func bindingResponses(list []*Binding) []BindingResponse {
	out := make([]BindingResponse, 0, len(list))
	for _, b := range list {
		out = append(out, bindingResponse(b))
	}
	return out
}

func memberResponse(m *Member) MemberResponse {
	return MemberResponse{ID: m.ID, Principal: m.Principal, Name: m.Name, Status: m.Status(), Guest: m.Guest}
}

func userResponse(u *User) UserResponse {
	return UserResponse{ID: u.ID, Name: u.Name, Status: u.Status}
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
	var maxUses any = unlimited
	if inv.MaxUses != nil {
		maxUses = *inv.MaxUses
	}
	return InviteResponse{
		ID: inv.ID, NodeID: inv.NodeID, Role: inv.Role, Grants: inv.Grants,
		MaxUses: maxUses, UseCount: inv.UseCount, Status: inv.Status, ExpiresAt: httpx.FormatTime(inv.ExpiresAt),
		TokenPrefix: inv.TokenPrefix, CreatedAt: httpx.FormatTime(inv.CreatedAt), Token: inv.Token,
	}
}

// --- errors ---

// mapErr maps the store's sentinels: anything not found under the address is
// 404, and a caller header naming no one is a malformed request.
func mapErr(c fiber.Ctx, err error, msg, resource, resourceID string) error {
	if err == ErrBadCaller {
		return errors.InvalidRequestError()
	}
	if err == nodes.ErrNotFound {
		return errors.NotFoundError("node", httpx.PathParam(c, "node_id"))
	}
	return httpx.MapDB(c.Context(), err, msg, httpx.DBErr{NotFound: ErrNotFound, Resource: resource, ResourceID: resourceID})
}

// header is a caller header, or "" when none was sent.
func header(c fiber.Ctx, name string) string {
	return strings.TrimSpace(strings.Clone(c.Get(name)))
}

// listStatus reads ?status=: active (or none) for live nodes, deleted for
// deleted ones.
func listStatus(c fiber.Ctx) (bool, error) {
	switch c.Query("status") {
	case "", nodes.StatusActive:
		return false, nil
	case nodes.StatusDeleted:
		return true, nil
	}
	return false, errors.FieldError("status", errors.FallbackValidation)
}

// --- nodes that read the caller ---

type CreateNodeRequest struct {
	DisplayName string  `json:"display_name"`
	Slug        *string `json:"slug"`
}

func (h *Handler) CreateChild(c fiber.Ctx) error {
	var req CreateNodeRequest
	if err := c.Bind().Body(&req); err != nil {
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
	if err := c.Bind().Body(&req); err != nil {
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

func (h *Handler) Roles(c fiber.Ctx) error {
	snap := h.src.Current()
	roles, err := h.store.Roles(c.Context(), snap, httpx.PathParam(c, "node_id"))
	if err != nil {
		return mapErr(c, err, "failed to list roles", "node", "")
	}
	out := RolesResponse{Roles: make([]RoleResponse, 0, len(roles)), CreatorRole: snap.CreatorRole}
	for _, r := range roles {
		out.Roles = append(out.Roles, RoleResponse{Slug: r.Slug, Permissions: r.Permissions})
	}
	return c.JSON(out)
}

// --- bindings ---

type CreateBindingRequest struct {
	MemberID string `json:"member_id"`
	Role     string `json:"role"`
}

type PatchBindingRequest struct {
	Role string `json:"role"`
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
	if err := c.Bind().Body(&req); err != nil {
		return err
	}
	b, err := h.store.CreateBinding(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"),
		strings.TrimSpace(req.MemberID), req.Role)
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
	before, after, err := h.store.PatchBinding(c.Context(), h.src.Current(), httpx.PathParam(c, "node_id"), bindingID, req.Role)
	if err != nil {
		return mapErr(c, err, "failed to update binding", "binding", bindingID)
	}
	d := auditx.Details{}
	d.Changed("role", before.Role, after.Role)
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
		auditx.Set(c, auditx.Details{"binding_id": b.ID, "member_id": b.MemberID, "role": auditx.Change{From: b.Role, To: nil}})
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
	if err := c.Bind().Body(&req); err != nil {
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
	if err := c.Bind().Body(&req); err != nil {
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
	if err := c.Bind().Body(&req); err != nil {
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
	if err := c.Bind().Body(&req); err != nil {
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
		Name string `json:"name"`
	}
	if len(c.Body()) > 0 {
		if err := c.Bind().Body(&req); err != nil {
			return err
		}
	}
	k, err := h.store.CreateKey(c.Context(), keyOwner(c), req.Name)
	if err != nil {
		return mapErr(c, err, "failed to create key", "key", "")
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
	Role      string          `json:"role"`
	Grants    []Grant         `json:"grants"`
	MaxUses   json.RawMessage `json:"max_uses"`
	ExpiresIn *string         `json:"expires_in"`
}

// read is the request with its defaults: max_uses omitted or null is 1,
// expires_in omitted is 7d.
func (r CreateInviteRequest) read() (InviteInput, error) {
	in := InviteInput{Role: r.Role, Grants: r.Grants, ExpiresIn: DefaultInviteExpiry}
	switch raw := strings.TrimSpace(string(r.MaxUses)); raw {
	case "", "null":
		one := 1
		in.MaxUses = &one
	case `"` + unlimited + `"`:
	default:
		var n int
		if err := json.Unmarshal(r.MaxUses, &n); err != nil || n < 1 {
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
	if err := c.Bind().Body(&req); err != nil {
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
	if err := c.Bind().Body(&req); err != nil {
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
