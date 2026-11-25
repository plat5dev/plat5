// Package feed is identity's copy of the deployment's config: the control
// plane's GET /internal/identity (docs/reference/api/internal.md#control-plane-feeds),
// swapped whole on each change.
package feed

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultDeletionGrace = 30 * 24 * time.Hour
	maxDurationSecs      = 36_500 * 86_400 // 100 years, safely inside int64 ns (~292y)
)

// Authorizer is the deployment-wide external OpenID AuthZEN 1.0 PDP.
type Authorizer struct {
	URL      string
	Timeout  time.Duration
	TokenEnv string
	Token    string
}

// Snapshot is one revision of the config. Read it once per request or sweep,
// so one piece of work sees one config.
type Snapshot struct {
	Revision      int64
	DeletionGrace time.Duration
	ReservedSlugs map[string]bool
	PolicyClaims  map[string]bool
	Kinds         map[string]Kind
	Roles         map[string]Role
	CreatorRole   string
	CustomRoles   bool
	Permissions   map[string]PermissionMeta
	Authorizer    *Authorizer

	// Precomputed role-binding indices built once via ensureRoleIndices().
	roleIdxOnce   sync.Once
	bindableKinds []string
	bindableRoles []string
	wildcardKinds []string
	wildcardRoles []string
	grantByPerm   map[string]kindRolePairs
}

type kindRolePairs struct {
	kinds []string
	roles []string
}

func (s *Snapshot) ensureRoleIndices() {
	s.roleIdxOnce.Do(s.rebuildRoleIndices)
}

// ExternalAuthz reports whether the deployment delegates all authorization
// decisions to an external AuthZEN 1.0 PDP instead of Plat5's built-in RBAC.
func (s *Snapshot) ExternalAuthz() bool {
	return s != nil && s.Authorizer != nil
}

// BindablePairs returns precomputed parallel (kind, role) arrays for every
// predefined role in snap that can be bound at a kind.
func (s *Snapshot) BindablePairs() (kinds, roles []string) {
	if s == nil {
		return []string{}, []string{}
	}
	s.ensureRoleIndices()
	if len(s.bindableKinds) == 0 {
		return []string{}, []string{}
	}
	return s.bindableKinds, s.bindableRoles
}

// GrantingPairs returns precomputed parallel (kind, role) arrays for every
// predefined role in snap that grants permission (directly, via includes, or "*").
func (s *Snapshot) GrantingPairs(permission string) (kinds, roles []string) {
	if s == nil {
		return []string{}, []string{}
	}
	s.ensureRoleIndices()
	p, ok := s.grantByPerm[permission]
	if !ok {
		if len(s.wildcardKinds) == 0 {
			return []string{}, []string{}
		}
		return s.wildcardKinds, s.wildcardRoles
	}
	if len(s.wildcardKinds) == 0 {
		return p.kinds, p.roles
	}
	k := make([]string, 0, len(p.kinds)+len(s.wildcardKinds))
	r := make([]string, 0, len(p.roles)+len(s.wildcardRoles))
	k = append(k, p.kinds...)
	k = append(k, s.wildcardKinds...)
	r = append(r, p.roles...)
	r = append(r, s.wildcardRoles...)
	return k, r
}

func (s *Snapshot) rebuildRoleIndices() {
	slugs := s.RoleSlugs()
	var bKinds, bRoles, wKinds, wRoles []string
	byPerm := map[string]kindRolePairs{}
	for _, slug := range slugs {
		role := s.Roles[slug]
		hasWildcard := false
		for _, perm := range role.Permissions {
			if perm == "*" {
				hasWildcard = true
				break
			}
		}
		for _, k := range role.BindableAt {
			bKinds = append(bKinds, k)
			bRoles = append(bRoles, slug)
			if hasWildcard {
				wKinds = append(wKinds, k)
				wRoles = append(wRoles, slug)
			} else {
				for _, perm := range role.Permissions {
					p := byPerm[perm]
					p.kinds = append(p.kinds, k)
					p.roles = append(p.roles, slug)
					byPerm[perm] = p
				}
			}
		}
	}
	s.bindableKinds = bKinds
	s.bindableRoles = bRoles
	s.wildcardKinds = wKinds
	s.wildcardRoles = wRoles
	s.grantByPerm = byPerm
}

// PermissionMeta describes one entry in the deployment's permission catalog
// for custom roles.
type PermissionMeta struct {
	Description          string
	BindableAt           []string
	CustomRoleAssignable bool
}

// PermissionCatalog returns the snapshot's permission catalog, falling back to
// permissions referenced by kinds and predefined roles (excluding "*") when
// the feed omitted the catalog.
func (s *Snapshot) PermissionCatalog() map[string]PermissionMeta {
	if len(s.Permissions) > 0 {
		return s.Permissions
	}
	out := make(map[string]PermissionMeta)
	for _, k := range s.Kinds {
		if k.ListPermission != "" && k.ListPermission != "*" {
			out[k.ListPermission] = PermissionMeta{CustomRoleAssignable: true}
		}
	}
	for _, r := range s.Roles {
		for _, p := range r.Permissions {
			if p != "*" {
				out[p] = PermissionMeta{CustomRoleAssignable: true}
			}
		}
	}
	return out
}

// Role is one entry of the roles document (docs/reference/config/roles.md).
type Role struct {
	Slug string
	// Permissions is what the role grants, its includes resolved, sorted, with
	// * as written.
	Permissions []string
	BindableAt  []string
}

// BindableAtKind is whether r can be bound at a node of kind.
func (r Role) BindableAtKind(kind string) bool {
	for _, k := range r.BindableAt {
		if k == kind {
			return true
		}
	}
	return false
}

// Role looks a role up by slug.
func (s *Snapshot) Role(slug string) (Role, bool) {
	r, ok := s.Roles[slug]
	return r, ok
}

// RoleSlugs is the role slugs, sorted.
func (s *Snapshot) RoleSlugs() []string {
	out := make([]string, 0, len(s.Roles))
	for slug := range s.Roles {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// Kind is one level of the tree (docs/reference/config/kinds.md).
type Kind struct {
	Name   string
	Parent string // empty for a root
	// ListPermission is what shows a node of this kind in a filtered list
	// (docs/concepts/access.md#lists).
	ListPermission string
	// BindCreator is whether creating a node of this kind gives its creator
	// creator_role (or a root member in external authorizer mode) on it
	// (docs/concepts/access.md#assigning).
	BindCreator bool
}

// Kind looks a kind up by name.
func (s *Snapshot) Kind(name string) (Kind, bool) {
	k, ok := s.Kinds[name]
	return k, ok
}

// IsRoot is whether nodes of k have no parent.
func (k Kind) IsRoot() bool {
	return k.Parent == ""
}

var (
	kindNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	durationRe = regexp.MustCompile(`^([1-9][0-9]{0,8})([smhd])$`)
)

type wireAuthorizer struct {
	URL      string `json:"url"`
	Timeout  string `json:"timeout"`
	TokenEnv string `json:"token_env"`
}

type wireKind struct {
	Parent         string `json:"parent"`
	ListPermission string `json:"list_permission"`
	// BindCreator is sent only when off: missing is on.
	BindCreator *bool `json:"bind_creator"`
}

type wireRole struct {
	Permissions []string `json:"permissions"`
	Includes    []string `json:"includes"`
	BindableAt  []string `json:"bindable_at"`
}

type wirePermissionMeta struct {
	Description          string   `json:"description"`
	BindableAt           []string `json:"bindable_at"`
	CustomRoleAssignable *bool    `json:"custom_role_assignable"`
}

type wireSnapshot struct {
	Revision      int64                         `json:"revision"`
	DeletionGrace string                        `json:"deletion_grace"`
	ReservedSlugs []string                      `json:"reserved_slugs"`
	PolicyClaims  []string                      `json:"policy_claims"`
	Kinds         map[string]wireKind           `json:"kinds"`
	Roles         map[string]wireRole           `json:"roles"`
	CreatorRole   string                        `json:"creator_role"`
	CustomRoles   bool                          `json:"custom_roles"`
	Permissions   map[string]wirePermissionMeta `json:"permissions"`
	Authorizer    *wireAuthorizer               `json:"authorizer"`
}

// Parse reads a feed body. The control plane validated the config at apply;
// this refuses only what identity can't work with, so a bad snapshot is
// skipped and the current one kept.
func Parse(body []byte) (*Snapshot, error) {
	var w wireSnapshot
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, err
	}
	grace := defaultDeletionGrace
	if w.DeletionGrace != "" {
		var err error
		grace, err = ParseDuration(w.DeletionGrace)
		if err != nil {
			return nil, fmt.Errorf("deletion_grace: %w", err)
		}
	}
	s := &Snapshot{
		Revision:      w.Revision,
		DeletionGrace: grace,
		ReservedSlugs: make(map[string]bool, len(w.ReservedSlugs)),
		PolicyClaims:  make(map[string]bool, len(w.PolicyClaims)),
		Kinds:         make(map[string]Kind, len(w.Kinds)),
		CreatorRole:   w.CreatorRole,
		CustomRoles:   w.CustomRoles,
		Permissions:   make(map[string]PermissionMeta, len(w.Permissions)),
	}
	for _, claim := range w.PolicyClaims {
		if c := strings.TrimSpace(claim); c != "" {
			s.PolicyClaims[c] = true
		}
	}
	if w.Authorizer != nil {
		u := strings.TrimRight(strings.TrimSpace(w.Authorizer.URL), "/")
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return nil, fmt.Errorf("authorizer.url: must be http or https")
		}
		timeout := 2 * time.Second
		if w.Authorizer.Timeout != "" {
			var err error
			timeout, err = ParseDuration(w.Authorizer.Timeout)
			if err != nil {
				return nil, fmt.Errorf("authorizer.timeout: %w", err)
			}
		}
		var token string
		if w.Authorizer.TokenEnv != "" {
			token = strings.TrimSpace(os.Getenv(w.Authorizer.TokenEnv))
			if token == "" {
				return nil, fmt.Errorf("authorizer.token_env references %s, which is not set", w.Authorizer.TokenEnv)
			}
		}
		s.Authorizer = &Authorizer{
			URL:      u,
			Timeout:  timeout,
			TokenEnv: w.Authorizer.TokenEnv,
			Token:    token,
		}
	}
	for perm, wm := range w.Permissions {
		if perm == "*" {
			continue
		}
		bindable := append([]string(nil), wm.BindableAt...)
		sort.Strings(bindable)
		s.Permissions[perm] = PermissionMeta{
			Description:          wm.Description,
			BindableAt:           bindable,
			CustomRoleAssignable: wm.CustomRoleAssignable == nil || *wm.CustomRoleAssignable,
		}
	}
	for _, slug := range w.ReservedSlugs {
		s.ReservedSlugs[slug] = true
	}
	if len(w.Kinds) == 0 {
		return nil, fmt.Errorf("kinds: none")
	}
	for name, wk := range w.Kinds {
		if !kindNameRe.MatchString(name) {
			return nil, fmt.Errorf("kinds: %q is not a kind name", name)
		}
		if wk.ListPermission == "" {
			return nil, fmt.Errorf("kinds.%s.list_permission: missing", name)
		}
		s.Kinds[name] = Kind{
			Name:           name,
			Parent:         wk.Parent,
			ListPermission: wk.ListPermission,
			BindCreator:    wk.BindCreator == nil || *wk.BindCreator,
		}
	}
	for name, k := range s.Kinds {
		if _, ok := s.Kinds[k.Parent]; !k.IsRoot() && !ok {
			return nil, fmt.Errorf("kinds.%s.parent: no kind %q", name, k.Parent)
		}
	}
	if s.Authorizer != nil && (len(w.Roles) > 0 || w.CreatorRole != "" || w.CustomRoles || len(w.Permissions) > 0) {
		return nil, fmt.Errorf("authorizer: cannot be combined with roles, creator_role, custom_roles, or permissions")
	}
	roles, err := resolveRoles(w.Roles)
	if err != nil {
		return nil, err
	}
	s.Roles = roles
	s.ensureRoleIndices()
	return s, nil
}

// resolveRoles reads the roles document with each role's includes resolved,
// transitively, into its permissions (docs/reference/config/roles.md).
func resolveRoles(wire map[string]wireRole) (map[string]Role, error) {
	out := make(map[string]Role, len(wire))
	for slug, wr := range wire {
		granted := map[string]bool{}
		seen := map[string]bool{}
		var walk func(slug string) error
		walk = func(slug string) error {
			if seen[slug] {
				return nil
			}
			seen[slug] = true
			r, ok := wire[slug]
			if !ok {
				return fmt.Errorf("roles: an included role %q isn't in the config", slug)
			}
			for _, p := range r.Permissions {
				granted[p] = true
			}
			for _, inc := range r.Includes {
				if err := walk(inc); err != nil {
					return err
				}
			}
			return nil
		}
		if err := walk(slug); err != nil {
			return nil, err
		}
		perms := make([]string, 0, len(granted))
		for p := range granted {
			perms = append(perms, p)
		}
		sort.Strings(perms)
		out[slug] = Role{Slug: slug, Permissions: perms, BindableAt: wr.BindableAt}
	}
	return out, nil
}

// ParseDuration reads the config's duration form: a positive whole number and
// s, m, h or d, capped at 36500d so Go's int64 time.Duration cannot overflow.
func ParseDuration(v string) (time.Duration, error) {
	m := durationRe.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("%q is not a duration", v)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a duration", v)
	}
	mult := map[string]int64{"s": 1, "m": 60, "h": 3600, "d": 86_400}[m[2]]
	if n > maxDurationSecs/mult {
		return 0, fmt.Errorf("%q exceeds maximum duration (36500d)", v)
	}
	return time.Duration(n*mult) * time.Second, nil
}

// Source holds the current snapshot. Nil until the first one is stored.
type Source struct {
	cur     atomic.Pointer[Snapshot]
	mu      sync.Mutex
	changed chan struct{}
}

// Static is a Source that holds s, for tests.
func Static(s *Snapshot) *Source {
	src := &Source{}
	src.Store(s)
	return src
}

func (src *Source) Store(s *Snapshot) {
	if s == nil {
		return
	}
	src.cur.Store(s)
	src.mu.Lock()
	if src.changed != nil {
		close(src.changed)
		src.changed = nil
	}
	src.mu.Unlock()
}

// Changed returns a channel closed the next time Store updates the snapshot.
func (src *Source) Changed() <-chan struct{} {
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.changed == nil {
		src.changed = make(chan struct{})
	}
	return src.changed
}

// Current is the snapshot in use, or nil before the first.
func (src *Source) Current() *Snapshot {
	return src.cur.Load()
}

// Loaded is whether identity has a config to serve with.
func (src *Source) Loaded() bool {
	return src.cur.Load() != nil
}

// Revision is the current snapshot's revision, or 0 before the first load.
func (src *Source) Revision() int64 {
	if s := src.cur.Load(); s != nil {
		return s.Revision
	}
	return 0
}
