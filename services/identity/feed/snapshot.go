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
	signIn        string
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

type wireSnapshot struct {
	Revision      int64               `json:"revision"`
	DeletionGrace string              `json:"deletion_grace"`
	ReservedSlugs []string            `json:"reserved_slugs"`
	Kinds         map[string]wireKind `json:"kinds"`
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
	return s, nil
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
