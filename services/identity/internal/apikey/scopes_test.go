package apikey

import "testing"

func TestWireScopes(t *testing.T) {
	if WireScopes(nil) != nil {
		t.Fatal("nil must wire as null")
	}
	empty := []string{}
	p := WireScopes(empty)
	if p == nil || len(*p) != 0 {
		t.Fatalf("empty must wire as empty array, got %#v", p)
	}
}

func TestValidLabel(t *testing.T) {
	for _, ok := range []string{"a", "org:members:write", "x.y_z-1"} {
		if !ValidLabel(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "*", "Upper", "has space", string(make([]byte, MaxScopeLen+1))} {
		if ValidLabel(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
}
