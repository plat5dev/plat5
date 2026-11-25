package members

import (
	"encoding/json"
	stderrors "errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
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
	r.Post("/nodes/:node_id/roles", h.CreateRole)
	r.Get("/nodes/:node_id/roles/:role_id", h.GetRole)
	r.Patch("/nodes/:node_id/roles/:role_id", h.PatchRole)
	r.Delete("/nodes/:node_id/roles/:role_id", h.DeleteRole)
	r.Get("/nodes/:node_id/permissions", h.ListPermissions)
	r.Get("/nodes/:node_id/security-policy", h.GetSecurityPolicy)
	r.Patch("/nodes/:node_id/security-policy", h.PatchSecurityPolicy)

	r.Get("/nodes/:node_id/bindings", h.ListBindings)
	r.Post("/nodes/:node_id/bindings", h.CreateBinding)
	r.Patch("/nodes/:node_id/bindings/:binding_id", h.PatchBinding)
	r.Delete("/nodes/:node_id/bindings/:binding_id", h.DeleteBinding)
	r.Get("/nodes/:node_id/memberships/:member_id", h.Membership)
	// The same as the members DELETE, so a public membership route reads and
	// leaves through one upstream. A root only, like it.
	r.Delete("/nodes/:node_id/memberships/:member_id", h.RemoveMember)

	r.Get("/nodes/:node_id/members", h.ListMembers)
	r.Post("/nodes/:node_id/members", h.AddMember)
	r.Get("/nodes/:node_id/members/:member_id", h.GetMember)
	r.Patch("/nodes/:node_id/members/:member_id/status", h.SetMemberStatus)
	r.Delete("/nodes/:node_id/members/:member_id", h.RemoveMember)
	r.Get("/members/:member_id/user", h.MemberUser)

	r.Get("/nodes/:node_id/groups", h.ListGroups)
	r.Post("/nodes/:node_id/groups", h.CreateGroup)
	r.Get("/nodes/:node_id/groups/:group_id", h.GetGroup)
	r.Patch("/nodes/:node_id/groups/:group_id", h.RenameGroup)
	r.Delete("/nodes/:node_id/groups/:group_id", h.DeleteGroup)
	r.Get("/nodes/:node_id/groups/:group_id/members", h.ListGroupMembers)
	r.Post("/nodes/:node_id/groups/:group_id/members", h.AddGroupMember)
	r.Delete("/nodes/:node_id/groups/:group_id/members/:member_id", h.RemoveGroupMember)

	r.Get("/users", h.LookupUser)
	r.Get("/users/:user_id", h.GetUser)
	r.Patch("/users/:user_id", h.SetName)
	r.Delete("/users/:user_id", h.DeleteAccount)
	r.Patch("/users/:user_id/status", h.SetStatus)
	r.Get("/users/:user_id/nodes", h.UserNodes)
	r.Get("/users/:user_id/keys", h.ListKeys)
	r.Post("/users/:user_id/keys", h.CreateKey)
	r.Patch("/users/:user_id/keys/:key_id", h.PatchKey)
	r.Delete("/users/:user_id/keys/:key_id", h.RevokeKey)
	r.Post("/users/:user_id/invites/redeem", h.Redeem)

	r.Get("/nodes/:node_id/service-accounts", h.ListServiceAccounts)
	r.Post("/nodes/:node_id/service-accounts", h.CreateServiceAccount)
	r.Get("/nodes/:node_id/service-accounts/:service_account_id", h.GetServiceAccount)
	r.Patch("/nodes/:node_id/service-accounts/:service_account_id", h.RenameServiceAccount)
	r.Delete("/nodes/:node_id/service-accounts/:service_account_id", h.DeleteServiceAccount)
	r.Get("/nodes/:node_id/service-accounts/:service_account_id/keys", h.ListKeys)
	r.Post("/nodes/:node_id/service-accounts/:service_account_id/keys", h.CreateKey)
	r.Patch("/nodes/:node_id/service-accounts/:service_account_id/keys/:key_id", h.PatchKey)
	r.Delete("/nodes/:node_id/service-accounts/:service_account_id/keys/:key_id", h.RevokeKey)

	r.Get("/nodes/:node_id/invites", h.ListInvites)
	r.Post("/nodes/:node_id/invites", h.CreateInvite)
	r.Delete("/nodes/:node_id/invites/:invite_id", h.RevokeInvite)
}

// --- shapes (docs/reference/api/identity.md#shapes) ---

type BindingResponse struct {
	ID        string  `json:"id"`
	MemberID  *string `json:"member_id"`
	GroupID   *string `json:"group_id"`
	NodeID    string  `json:"node_id"`
	Role      string  `json:"role"`
	Inherited bool    `json:"inherited"`
	Guest     bool    `json:"guest"`
	Principal string  `json:"principal"`
	Name      *string `json:"name"`
	ExpiresAt *string `json:"expires_at"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

type GroupResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	RootID    string `json:"root_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type MembershipResponse struct {
	MemberID    string             `json:"member_id"`
	Status      string             `json:"status"`
	Bindings    *[]BindingResponse `json:"bindings,omitempty"`
	Permissions *[]string          `json:"permissions,omitempty"`
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
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	KeyPrefix    string   `json:"key_prefix"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
	Scopes       []string `json:"scopes"`
	AllowedRoots []string `json:"allowed_roots"`
	ExpiresAt    *string  `json:"expires_at"`
	CreatedAt    string   `json:"created_at"`
	RevokedAt    *string  `json:"revoked_at"`
	Key          string   `json:"key,omitempty"`
}

type InviteResponse struct {
	ID          string  `json:"id"`
	NodeID      string  `json:"node_id"`
	Role        string  `json:"role"`
	Grants      []Grant `json:"grants"`
	MaxUses     *int    `json:"max_uses"` // null: unlimited
	UseCount    int     `json:"use_count"`
	Status      string  `json:"status"`
	ExpiresAt   string  `json:"expires_at"`
	TokenPrefix string  `json:"token_prefix"`
	CreatedAt   string  `json:"created_at"`
	Token       string  `json:"token,omitempty"`
}

type RoleResponse struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	DisplayName string   `json:"display_name"`
	Description string   `json:"description,omitempty"`
	Source      string   `json:"source"`
	Permissions []string `json:"permissions"`
	BindableAt  []string `json:"bindable_at"`
	CreatedAt   *string  `json:"created_at,omitempty"`
	UpdatedAt   *string  `json:"updated_at,omitempty"`
}

type RolesResponse struct {
	Roles       []RoleResponse `json:"roles"`
	CreatorRole string         `json:"creator_role"`
}

type SecurityPolicyResponse struct {
	AllowPersonalKeys bool                `json:"allow_personal_keys"`
	AllowedCIDRs      []string            `json:"allowed_cidrs"`
	RequiredIdPClaims map[string][]string `json:"required_idp_claims"`
	UpdatedAt         *string             `json:"updated_at"`
}

type RedeemResponse struct {
	MemberID string             `json:"member_id"`
	Node     nodes.NodeResponse `json:"node"`
}

func roleResponse(r RoleView) RoleResponse {
	return RoleResponse{
		ID:          r.ID,
		Slug:        r.Slug,
		DisplayName: r.DisplayName,
		Description: r.Description,
		Source:      r.Source,
		Permissions: r.Permissions,
		BindableAt:  r.BindableAt,
		CreatedAt:   httpx.FormatTimePtr(r.CreatedAt),
		UpdatedAt:   httpx.FormatTimePtr(r.UpdatedAt),
	}
}

func securityPolicyResponse(p *RootSecurityPolicy) SecurityPolicyResponse {
	cidrs := p.AllowedCIDRs
	if cidrs == nil {
		cidrs = []string{}
	}
	claims := p.RequiredIdPClaims
	if claims == nil {
		claims = map[string][]string{}
	}
	return SecurityPolicyResponse{
		AllowPersonalKeys: p.AllowPersonalKeys,
		AllowedCIDRs:      cidrs,
		RequiredIdPClaims: claims,
		UpdatedAt:         httpx.FormatTimePtr(p.UpdatedAt),
	}
}

func bindingResponse(b *Binding) BindingResponse {
	return BindingResponse{
		ID: b.ID, MemberID: b.MemberID, GroupID: b.GroupID, NodeID: b.NodeID, Role: b.Role, Inherited: b.Inherited, Guest: b.Guest,
		Principal: b.Principal, Name: b.Name, ExpiresAt: httpx.FormatTimePtr(b.ExpiresAt),
		CreatedAt: httpx.FormatTime(b.CreatedAt), UpdatedAt: httpx.FormatTime(b.UpdatedAt),
	}
}

func groupResponse(g *Group) GroupResponse {
	return GroupResponse{
		ID: g.ID, Name: g.Name, RootID: g.RootID,
		CreatedAt: httpx.FormatTime(g.CreatedAt), UpdatedAt: httpx.FormatTime(g.UpdatedAt),
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
	cidrs := k.AllowedCIDRs
	if cidrs == nil {
		cidrs = []string{}
	}
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	roots := k.AllowedRoots
	if roots == nil {
		roots = []string{}
	}
	return KeyResponse{
		ID: k.ID, Name: k.Name, KeyPrefix: k.KeyPrefix,
		AllowedCIDRs: cidrs, Scopes: scopes, AllowedRoots: roots,
		ExpiresAt: httpx.FormatTimePtr(k.ExpiresAt),
		CreatedAt: httpx.FormatTime(k.CreatedAt), RevokedAt: httpx.FormatTimePtr(k.RevokedAt),
		Key: k.Secret,
	}
}

func inviteResponse(inv *Invite) InviteResponse {
	return InviteResponse{
		ID: inv.ID, NodeID: inv.NodeID, Role: inv.Role, Grants: inv.Grants,
		MaxUses: inv.MaxUses, UseCount: inv.UseCount, Status: inv.Status, ExpiresAt: httpx.FormatTime(inv.ExpiresAt),
		TokenPrefix: inv.TokenPrefix, CreatedAt: httpx.FormatTime(inv.CreatedAt), Token: inv.Token,
	}
}

// --- shared handler helpers ---

// mapErr maps the store's sentinels: anything not found under the address is
// 404, and a caller header naming no one is a malformed request.
func mapErr(c fiber.Ctx, err error, msg, resource, resourceID string) error {
	if stderrors.Is(err, ErrBadCaller) {
		return errors.InvalidRequestError()
	}
	if stderrors.Is(err, nodes.ErrNotFound) {
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

// parseExpiry reads optional expires_at (RFC 3339 or null) and expires_in
// (duration string like 7d), mutually exclusive. Returns set=true when either
// field was sent.
func parseExpiry(expiresAt, expiresIn httpx.PatchField, now time.Time) (set bool, val *time.Time, err error) {
	if expiresAt.Set && expiresIn.Set {
		return false, nil, errors.FieldError("expires_in", "Send either expires_at or expires_in, not both.")
	}
	if !expiresAt.Set && !expiresIn.Set {
		return false, nil, nil
	}
	if expiresAt.Set {
		if expiresAt.IsNull() {
			return true, nil, nil
		}
		var raw string
		if err := json.Unmarshal(expiresAt.Raw, &raw); err != nil {
			return false, nil, errors.FieldError("expires_at", "Must be an RFC 3339 timestamp in the future.")
		}
		t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
		if err != nil || !t.After(now) {
			return false, nil, errors.FieldError("expires_at", "Must be an RFC 3339 timestamp in the future.")
		}
		utc := t.UTC()
		return true, &utc, nil
	}
	if expiresIn.IsNull() {
		return false, nil, errors.FieldError("expires_in", "Must be a positive duration such as 1h or 7d.")
	}
	var raw string
	if err := json.Unmarshal(expiresIn.Raw, &raw); err != nil || strings.TrimSpace(raw) == "" {
		return false, nil, errors.FieldError("expires_in", "Must be a positive duration such as 1h or 7d.")
	}
	d, err := feed.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d < time.Second {
		return false, nil, errors.FieldError("expires_in", "Must be a positive duration such as 1h or 7d.")
	}
	utc := now.UTC().Add(d)
	return true, &utc, nil
}
