// Package roles is the deployment's roles file: role slug → labels.
//
// Plat5 names no roles. A nil *Set means there is no roles file, and every
// member is unrestricted. Contract: docs/roles.md.
package roles

import (
	stderrors "errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v2"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/apikey"
)

// Wildcard grants every label. Valid only alone. It does not leave identity:
// on the wire, every label is a null scopes list.
const Wildcard = "*"

// MaxLabels caps one role's label list. A member's effective set travels on
// X-Plat5-Scopes, so it shares that bound.
const MaxLabels = apikey.MaxCallerScopes

// Role is one entry of the file. Scopes nil is every label.
type Role struct {
	Slug   string
	Scopes []string
}

// Set is the parsed roles file.
type Set struct {
	grants  map[string][]string
	creator string
	def     string
}

type file struct {
	Roles       map[string][]string `yaml:"roles"`
	CreatorRole string              `yaml:"creator_role"`
	DefaultRole string              `yaml:"default_role"`
}

// Load reads ROLES_FILE. An empty path is no roles file (nil, nil).
func Load(path string) (*Set, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read roles file: %w", err)
	}
	return Parse(data)
}

// Parse validates a roles file. Any problem refuses boot.
func Parse(data []byte) (*Set, error) {
	var f file
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, fmt.Errorf("roles file: %w", err)
	}
	if len(f.Roles) == 0 {
		return nil, stderrors.New("roles file: roles must define at least one role")
	}

	s := &Set{grants: make(map[string][]string, len(f.Roles))}
	for slug, labels := range f.Roles {
		if !apikey.ValidLabel(slug) {
			return nil, fmt.Errorf("roles file: role %q: slug must be [a-z0-9:._-]+, max %d", slug, apikey.MaxScopeLen)
		}
		grant, err := parseLabels(labels)
		if err != nil {
			return nil, fmt.Errorf("roles file: role %q: %w", slug, err)
		}
		s.grants[slug] = grant
	}

	if f.CreatorRole == "" {
		return nil, stderrors.New("roles file: creator_role is required")
	}
	if _, ok := s.grants[f.CreatorRole]; !ok {
		return nil, fmt.Errorf("roles file: creator_role %q is not a role", f.CreatorRole)
	}
	if f.DefaultRole == "" {
		return nil, stderrors.New("roles file: default_role is required")
	}
	if _, ok := s.grants[f.DefaultRole]; !ok {
		return nil, fmt.Errorf("roles file: default_role %q is not a role", f.DefaultRole)
	}
	s.creator = f.CreatorRole
	s.def = f.DefaultRole
	return s, nil
}

// parseLabels maps a file list to a grant. ["*"] is nil (every label).
// A missing list is refused so `member:` is not read as "everything" or "nothing".
func parseLabels(labels []string) ([]string, error) {
	if labels == nil {
		return nil, stderrors.New(`labels are missing; use [] for none or ["*"] for every label`)
	}
	if len(labels) == 1 && labels[0] == Wildcard {
		return nil, nil
	}
	if len(labels) > MaxLabels {
		return nil, fmt.Errorf("more than %d labels", MaxLabels)
	}
	seen := make(map[string]struct{}, len(labels))
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == Wildcard {
			return nil, stderrors.New(`"*" is only valid alone`)
		}
		if !apikey.ValidLabel(l) {
			return nil, fmt.Errorf("label %q must be [a-z0-9:._-]+, max %d", l, apikey.MaxScopeLen)
		}
		if _, dup := seen[l]; dup {
			return nil, fmt.Errorf("label %q is repeated", l)
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out, nil
}

// Creator is the role an org's creator gets. Nil without a roles file.
func (s *Set) Creator() *string {
	if s == nil {
		return nil
	}
	c := s.creator
	return &c
}

// Default is the role a write gets when it omits one. Nil without a roles file.
func (s *Set) Default() *string {
	if s == nil {
		return nil
	}
	d := s.def
	return &d
}

// Grants is what a member's role grants. A nil role, or no roles file, is every
// label (nil). A slug no longer in the file grants nothing.
func (s *Set) Grants(role *string) []string {
	if s == nil || role == nil {
		return nil
	}
	grant, ok := s.grants[*role]
	if !ok {
		return []string{}
	}
	return grant
}

// Resolve is a member credential's effective scopes: role grant ∩ credential scopes.
func (s *Set) Resolve(role *string, credential []string) []string {
	return apikey.Intersect(s.Grants(role), credential)
}

// List is the file's roles, sorted by slug. Empty without a roles file.
func (s *Set) List() []Role {
	if s == nil {
		return []Role{}
	}
	out := make([]Role, 0, len(s.grants))
	for slug, grant := range s.grants {
		out = append(out, Role{Slug: slug, Scopes: grant})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// Choose validates `role` from a request body. Omitted (or null) is default_role.
// Any role without a roles file, or a slug not in it, is 422.
func (s *Set) Choose(raw *string) (*string, error) {
	if raw == nil {
		return s.Default(), nil
	}
	if s == nil {
		return nil, errors.FieldError("role", "Roles aren't set up for this deployment.")
	}
	slug := strings.TrimSpace(*raw)
	if _, ok := s.grants[slug]; !ok {
		return nil, errors.FieldError("role", "That role doesn't exist.")
	}
	return &slug, nil
}

// CheckAssign refuses a role with a label the caller lacks. caller nil is unrestricted.
func (s *Set) CheckAssign(caller []string, role *string) error {
	if s == nil || role == nil {
		return nil
	}
	if missing := Missing(caller, s.Grants(role)); len(missing) > 0 {
		return errors.InsufficientRole("You can't assign the "+*role+" role.", missing)
	}
	return nil
}

// CheckActOn refuses acting on a member whose role has a label the caller lacks.
// Without a roles file there is nothing to protect: the key mint cap is the only cap.
func (s *Set) CheckActOn(caller []string, role *string) error {
	if s == nil {
		return nil
	}
	missing := Missing(caller, s.Grants(role))
	if len(missing) == 0 {
		return nil
	}
	if role == nil {
		return errors.InsufficientRole("You can't change an unrestricted member.", missing)
	}
	return errors.InsufficientRole("You can't change a member with the "+*role+" role.", missing)
}

// Missing lists the labels in need the caller does not hold. A nil caller holds
// every label. A nil need is every label, which a restricted caller is missing as "*".
func Missing(caller, need []string) []string {
	if caller == nil {
		return nil
	}
	if need == nil {
		return []string{Wildcard}
	}
	have := make(map[string]struct{}, len(caller))
	for _, s := range caller {
		have[s] = struct{}{}
	}
	var missing []string
	for _, s := range need {
		if _, ok := have[s]; !ok {
			missing = append(missing, s)
		}
	}
	return missing
}
