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

// Authorizer modes combining external AuthZEN PDPs with built-in RBAC.
const (
	AuthorizerModeReplace = "replace"
	AuthorizerModeAnd     = "and"
	AuthorizerModeOr      = "or"
)

// Authorizer is an external OpenID AuthZEN 1.0 PDP configured in authorizers.
type Authorizer struct {
	Name     string
	URL      string
	Timeout  time.Duration
	TokenEnv string
	Token    string
}

// KindAuthorizer attaches a named external authorizer to a kind.
type KindAuthorizer struct {
	Name string
	Mode string
}

// Snapshot is one revision of the config. Read it once per request or sweep,
// so one piece of work sees one config.
type Snapshot struct {
	Revision      int64
	DeletionGrace time.Duration
	ReservedSlugs map[string]bool
	Kinds         map[string]Kind
	Roles         map[string]Role
	CreatorRole   string
	CustomRoles   bool
	Permissions   map[string]PermissionMeta
	Authorizers   map[string]Authorizer
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
	// creator_role on it (docs/concepts/access.md#assigning).
	BindCreator bool
	// AllowPublic is whether nodes of this kind accept public principal
	// bindings (all_users, all_authenticated_users).
	AllowPublic bool
	// Authorizer is an optional external AuthZEN PDP for list_permission and
	// identity /access/v1/* evaluation on nodes of this kind.
	Authorizer *KindAuthorizer
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

// Names is the kind names, sorted.
func (s *Snapshot) Names() []string {
	out := make([]string, 0, len(s.Kinds))
	for name := range s.Kinds {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

var (
	kindNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	durationRe = regexp.MustCompile(`^([1-9][0-9]{0,8})([smhd])$`)
)

type wireKindAuthorizer struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

type wireAuthorizer struct {
	URL      string `json:"url"`
	Timeout  string `json:"timeout"`
	TokenEnv string `json:"token_env"`
}

type wireKind struct {
	Parent         string `json:"parent"`
	ListPermission string `json:"list_permission"`
	// BindCreator is sent only when off: missing is on.
	BindCreator *bool               `json:"bind_creator"`
	AllowPublic bool                `json:"allow_public"`
	Authorizer  *wireKindAuthorizer `json:"authorizer"`
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
	Kinds         map[string]wireKind           `json:"kinds"`
	Roles         map[string]wireRole           `json:"roles"`
	CreatorRole   string                        `json:"creator_role"`
	CustomRoles   bool                          `json:"custom_roles"`
	Permissions   map[string]wirePermissionMeta `json:"permissions"`
	Authorizers   map[string]wireAuthorizer     `json:"authorizers"`
}

// Parse reads a feed body. The control plane validated the config at apply;
// this refuses only what identity can't work with, so a bad snapshot is
// skipped and the current one kept.
func Parse(body []byte) (*Snapshot, error) {
	var w wireSnapshot
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, err
	}
	grace, err := ParseDuration(w.DeletionGrace)
	if err != nil {
		return nil, fmt.Errorf("deletion_grace: %w", err)
	}
	s := &Snapshot{
		Revision:      w.Revision,
		DeletionGrace: grace,
		ReservedSlugs: make(map[string]bool, len(w.ReservedSlugs)),
		Kinds:         make(map[string]Kind, len(w.Kinds)),
		CreatorRole:   w.CreatorRole,
		CustomRoles:   w.CustomRoles,
		Permissions:   make(map[string]PermissionMeta, len(w.Permissions)),
		Authorizers:   make(map[string]Authorizer, len(w.Authorizers)),
	}
	for name, wa := range w.Authorizers {
		u := strings.TrimRight(strings.TrimSpace(wa.URL), "/")
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return nil, fmt.Errorf("authorizers.%s.url: must be http or https", name)
		}
		timeout := 2 * time.Second
		if wa.Timeout != "" {
			var err error
			timeout, err = ParseDuration(wa.Timeout)
			if err != nil {
				return nil, fmt.Errorf("authorizers.%s.timeout: %w", name, err)
			}
		}
		var token string
		if wa.TokenEnv != "" {
			token = strings.TrimSpace(os.Getenv(wa.TokenEnv))
			if token == "" {
				return nil, fmt.Errorf("authorizers.%s.token_env references %s, which is not set", name, wa.TokenEnv)
			}
		}
		s.Authorizers[name] = Authorizer{
			Name:     name,
			URL:      u,
			Timeout:  timeout,
			TokenEnv: wa.TokenEnv,
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
		var ka *KindAuthorizer
		if wk.Authorizer != nil {
			if _, ok := s.Authorizers[wk.Authorizer.Name]; !ok {
				return nil, fmt.Errorf("kinds.%s.authorizer: %q is not in authorizers", name, wk.Authorizer.Name)
			}
			mode := wk.Authorizer.Mode
			if mode == "" {
				mode = AuthorizerModeReplace
			}
			if mode != AuthorizerModeReplace && mode != AuthorizerModeAnd && mode != AuthorizerModeOr {
				return nil, fmt.Errorf("kinds.%s.authorizer.mode: invalid mode %q", name, mode)
			}
			ka = &KindAuthorizer{Name: wk.Authorizer.Name, Mode: mode}
		}
		s.Kinds[name] = Kind{
			Name:           name,
			Parent:         wk.Parent,
			ListPermission: wk.ListPermission,
			BindCreator:    wk.BindCreator == nil || *wk.BindCreator,
			AllowPublic:    wk.AllowPublic,
			Authorizer:     ka,
		}
	}
	for name, k := range s.Kinds {
		if _, ok := s.Kinds[k.Parent]; !k.IsRoot() && !ok {
			return nil, fmt.Errorf("kinds.%s.parent: no kind %q", name, k.Parent)
		}
	}
	roles, err := resolveRoles(w.Roles)
	if err != nil {
		return nil, err
	}
	s.Roles = roles
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

// ParseDuration reads the config's duration form: a whole number and s, m, h
// or d. Empty is the default, 30d.
func ParseDuration(v string) (time.Duration, error) {
	if v == "" {
		return 30 * 24 * time.Hour, nil
	}
	m := durationRe.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("%q is not a duration", v)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, nil
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
