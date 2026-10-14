package roles

import (
	"reflect"
	"testing"
)

func TestSourceOff(t *testing.T) {
	for _, s := range []*Source{nil, Off(), Static(nil)} {
		if s.Enabled() || !s.Loaded() || s.Set() != nil {
			t.Fatalf("off: enabled=%v loaded=%v set=%v", s.Enabled(), s.Loaded(), s.Set())
		}
	}
}

// Roles on with no document yet must never read as roles off: off grants
// every label.
func TestSourceOnBeforeLoadFailsClosed(t *testing.T) {
	s := On()
	if !s.Enabled() || s.Loaded() {
		t.Fatal("on, not loaded")
	}
	set := s.Set()
	if set == nil {
		t.Fatal("an unloaded source must not be roles off")
	}
	if got := set.Grants(strp("owner")); len(got) != 0 {
		t.Fatalf("no document grants nothing, got %#v", got)
	}
	if _, err := set.Choose(strp("owner")); err == nil {
		t.Fatal("no document has no roles to choose")
	}
}

func TestSourceStoreSwaps(t *testing.T) {
	s := On()
	s.Store(mustParse(t, starter))
	if !s.Loaded() {
		t.Fatal("loaded after store")
	}
	if got := s.Set().Grants(strp("owner")); !reflect.DeepEqual(got, []string{"*"}) {
		t.Fatalf("owner: %#v", got)
	}
	s.Store(mustParse(t, "roles: {owner: [x]}\ncreator_role: owner\ndefault_role: owner\n"))
	if got := s.Set().Grants(strp("owner")); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("swapped owner: %#v", got)
	}
	s.Store(nil)
	if s.Set() == nil {
		t.Fatal("an on source never turns off")
	}
	off := Off()
	off.Store(mustParse(t, starter))
	if off.Set() != nil {
		t.Fatal("storing into an off source does nothing")
	}
}
