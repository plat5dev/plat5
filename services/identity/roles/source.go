package roles

import "sync/atomic"

// Source is where identity reads the deployment's roles. Whether roles are on
// is a boot switch (ROLES_ENABLED). With roles on, the set is the control
// plane's roles document, swapped whole on each change (docs/roles.md). A nil
// *Source is roles off.
type Source struct {
	on  bool
	cur atomic.Pointer[Set]
}

// closed is what an on Source holds before its first document: no roles, so
// every member gets no labels and every role in a body is refused. Identity
// does not serve before the first document; this only keeps a mistake from
// reading as roles off, which would grant every label.
var closed = &Set{grants: map[string][]string{}}

// Off is roles off for the whole deployment.
func Off() *Source {
	return &Source{}
}

// On is roles on, with no document yet. Store fills it.
func On() *Source {
	return &Source{on: true}
}

// Static never changes: roles on with set, or off when set is nil.
func Static(set *Set) *Source {
	if set == nil {
		return Off()
	}
	s := On()
	s.Store(set)
	return s
}

// Enabled is whether roles are on.
func (s *Source) Enabled() bool {
	return s != nil && s.on
}

// Loaded is whether identity can answer with roles: off, or on with a document.
func (s *Source) Loaded() bool {
	return !s.Enabled() || s.cur.Load() != nil
}

// Store replaces the current set. A nil set is ignored: an on Source never
// turns off.
func (s *Source) Store(set *Set) {
	if set != nil && s.Enabled() {
		s.cur.Store(set)
	}
}

// Set is the current roles. Nil only when roles are off. Read it once per
// request, so one request sees one document.
func (s *Source) Set() *Set {
	if !s.Enabled() {
		return nil
	}
	if set := s.cur.Load(); set != nil {
		return set
	}
	return closed
}
