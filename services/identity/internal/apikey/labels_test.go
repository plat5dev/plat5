package apikey

import "testing"

func TestWireLabels(t *testing.T) {
	if WireLabels(nil) != nil {
		t.Fatal("nil must wire as null")
	}
	empty := []string{}
	p := WireLabels(empty)
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
	for _, bad := range []string{"", "*", "Upper", "has space", string(make([]byte, MaxLabelLen+1))} {
		if ValidLabel(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
}
