// Package feed is identity's copy of the deployment's config: the control
// plane's GET /internal/identity (docs/reference/api/internal.md#control-plane-feeds),
// swapped whole on each change.
package feed

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"sync/atomic"
	"time"
)

// Snapshot is one revision of the config. Read it once per request or sweep,
// so one piece of work sees one config.
type Snapshot struct {
	Revision      int64
	DeletionGrace time.Duration
	ReservedSlugs map[string]bool
	Kinds         map[string]Kind
	Roles         map[string]Role
	CreatorRole   string
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

type wireKind struct {
	Parent         string `json:"parent"`
	ListPermission string `json:"list_permission"`
}

type wireRole struct {
	Permissions []string `json:"permissions"`
	Includes    []string `json:"includes"`
	BindableAt  []string `json:"bindable_at"`
}

type wireSnapshot struct {
	Revision      int64               `json:"revision"`
	DeletionGrace string              `json:"deletion_grace"`
	ReservedSlugs []string            `json:"reserved_slugs"`
	Kinds         map[string]wireKind `json:"kinds"`
	Roles         map[string]wireRole `json:"roles"`
	CreatorRole   string              `json:"creator_role"`
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
		s.Kinds[name] = Kind{Name: name, Parent: wk.Parent, ListPermission: wk.ListPermission}
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
	cur atomic.Pointer[Snapshot]
}

// Static is a Source that holds s, for tests.
func Static(s *Snapshot) *Source {
	src := &Source{}
	src.Store(s)
	return src
}

func (src *Source) Store(s *Snapshot) {
	if s != nil {
		src.cur.Store(s)
	}
}

// Current is the snapshot in use, or nil before the first.
func (src *Source) Current() *Snapshot {
	return src.cur.Load()
}

// Loaded is whether identity has a config to serve with.
func (src *Source) Loaded() bool {
	return src.cur.Load() != nil
}
