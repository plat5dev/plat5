package apikey

import "testing"

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
