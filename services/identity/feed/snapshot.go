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
	DefaultRole   string
	CreatorRole   string
	// readRoutes and deleteRoutes are the labels of identity's own GET and
	// DELETE routes to /nodes/{subject.<kind>_id}, per kind, which filter node
	// lists (docs/concepts/access.md#lists).
	readRoutes   map[string]RouteLabels
	deleteRoutes map[string]RouteLabels
	signIn       string
}

// Role is one entry of the roles document (docs/reference/config/roles.md).
type Role struct {
	Slug        string
	Labels      []string
	BindableAt  []string
	MaxDuration time.Duration // zero: none
	// MaxDurationText is max_duration as written, for messages.
	MaxDurationText string
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

// RouteLabels is what a kind's applied routes of one sort require: Open when
// any of them is [] (any member of the tree), else any one of Labels.
type RouteLabels struct {
	Open   bool
	Labels []string
}

// IdentityService is the service in the config whose routes are identity's
// own. The generated catalog names it so.
const IdentityService = "identity"

// ReadLabels is what reading a node of kind takes. ok is false when no read
// route is applied: then nothing of that kind is readable.
func (s *Snapshot) ReadLabels(kind string) (RouteLabels, bool) {
	r, ok := s.readRoutes[kind]
	return r, ok
}

// DeleteLabels is ReadLabels for the kind's delete route, which governs
// listing deleted nodes.
func (s *Snapshot) DeleteLabels(kind string) (RouteLabels, bool) {
	r, ok := s.deleteRoutes[kind]
	return r, ok
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
	Name    string
	Parents []string // empty for a root
	Label   string
	SignIn  bool
}

// SignInKind is the one kind marked sign_in.
func (s *Snapshot) SignInKind() Kind {
	return s.Kinds[s.signIn]
}

// Kind looks a kind up by name.
func (s *Snapshot) Kind(name string) (Kind, bool) {
	k, ok := s.Kinds[name]
	return k, ok
}

// IsRoot is whether nodes of k have no parent.
func (k Kind) IsRoot() bool {
	return len(k.Parents) == 0
}

// AllowsParent is whether a node of k may sit under a node of parentKind.
func (k Kind) AllowsParent(parentKind string) bool {
	for _, p := range k.Parents {
		if p == parentKind {
			return true
		}
	}
	return false
}

// DisplayLabel is the kind's label, or its name when it has none.
func (k Kind) DisplayLabel() string {
	if k.Label != "" {
		return k.Label
	}
	return k.Name
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
	Parent json.RawMessage `json:"parent"`
	Label  string          `json:"label"`
	SignIn string          `json:"sign_in"`
}

type wireRole struct {
	Labels      []string `json:"labels"`
	BindableAt  []string `json:"bindable_at"`
	MaxDuration *string  `json:"max_duration"`
}

type wireRoute struct {
	Methods        []string  `json:"methods"`
	Upstream       *string   `json:"upstream"`
	RequiredLabels *[]string `json:"required_labels"`
}

type wireService struct {
	Routes []wireRoute `json:"routes"`
}

type wireSnapshot struct {
	Revision      int64                  `json:"revision"`
	DeletionGrace string                 `json:"deletion_grace"`
	ReservedSlugs []string               `json:"reserved_slugs"`
	Kinds         map[string]wireKind    `json:"kinds"`
	Roles         map[string]wireRole    `json:"roles"`
	DefaultRole   string                 `json:"default_role"`
	CreatorRole   string                 `json:"creator_role"`
	Services      map[string]wireService `json:"services"`
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
		parents, err := parseParents(wk.Parent)
		if err != nil {
			return nil, fmt.Errorf("kinds.%s.parent: %w", name, err)
		}
		k := Kind{Name: name, Parents: parents, Label: wk.Label, SignIn: wk.SignIn != ""}
		if k.SignIn {
			if s.signIn != "" {
				return nil, fmt.Errorf("kinds: %s and %s both have sign_in", s.signIn, name)
			}
			if !k.IsRoot() {
				return nil, fmt.Errorf("kinds.%s: the sign-in kind has a parent", name)
			}
			s.signIn = name
		}
		s.Kinds[name] = k
	}
	if s.signIn == "" {
		return nil, fmt.Errorf("kinds: none has sign_in")
	}
	for name, k := range s.Kinds {
		for _, p := range k.Parents {
			if _, ok := s.Kinds[p]; !ok {
				return nil, fmt.Errorf("kinds.%s.parent: no kind %q", name, p)
			}
		}
	}
	if err := s.parseRoles(w); err != nil {
		return nil, err
	}
	s.readRoutes, s.deleteRoutes = nodeRoutes(w.Services[IdentityService])
	return s, nil
}

func (s *Snapshot) parseRoles(w wireSnapshot) error {
	s.Roles = make(map[string]Role, len(w.Roles))
	for slug, wr := range w.Roles {
		r := Role{Slug: slug, Labels: wr.Labels, BindableAt: wr.BindableAt}
		if wr.MaxDuration != nil {
			d, err := ParseDuration(*wr.MaxDuration)
			if err != nil || *wr.MaxDuration == "" {
				return fmt.Errorf("roles.%s.max_duration: %q is not a duration", slug, *wr.MaxDuration)
			}
			r.MaxDuration, r.MaxDurationText = d, *wr.MaxDuration
		}
		s.Roles[slug] = r
	}
	s.DefaultRole, s.CreatorRole = w.DefaultRole, w.CreatorRole
	return nil
}

var nodeUpstreamRe = regexp.MustCompile(`^/nodes/\{subject\.([a-z][a-z0-9_]*)_id\}$`)

// nodeRoutes reads, from identity's own routes, the labels of each kind's
// GET and DELETE to /nodes/{subject.<kind>_id}. Several routes add up: any of
// their labels counts.
func nodeRoutes(svc wireService) (read, del map[string]RouteLabels) {
	read, del = map[string]RouteLabels{}, map[string]RouteLabels{}
	add := func(m map[string]RouteLabels, kind string, labels *[]string) {
		r := m[kind]
		if labels == nil || len(*labels) == 0 {
			r.Open = true
		} else {
			r.Labels = append(r.Labels, *labels...)
		}
		m[kind] = r
	}
	for _, route := range svc.Routes {
		if route.Upstream == nil {
			continue
		}
		m := nodeUpstreamRe.FindStringSubmatch(*route.Upstream)
		if m == nil {
			continue
		}
		for _, method := range route.Methods {
			switch method {
			case "GET":
				add(read, m[1], route.RequiredLabels)
			case "DELETE":
				add(del, m[1], route.RequiredLabels)
			}
		}
	}
	return read, del
}

func parseParents(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("a kind or a list of kinds")
	}
	return many, nil
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
