package id

import "testing"

func TestNewIsValid(t *testing.T) {
	n := New()
	if !Valid(n) || !IsID(n) {
		t.Fatalf("%q should be an id", n)
	}
}

func TestValidRejects(t *testing.T) {
	for _, s := range []string{"", "01J0000000000000000000000Z", "n_01j00000000000000000000000", "not-a-ulid", "gruow", "01j0000000000000000000000"} {
		if Valid(s) {
			t.Fatalf("%q should not be an id", s)
		}
	}
	if !Valid("01jbx9q3t5vk8m2n4p6r7s8t9w") {
		t.Fatal("a lowercase ULID is an id")
	}
}

func TestIsID(t *testing.T) {
	for ref, want := range map[string]bool{"01jbx9q3t5vk8m2n4p6r7s8t9w": true, "9x": true, "gruow": false, "": false} {
		if IsID(ref) != want {
			t.Fatalf("IsID(%q) = %v", ref, !want)
		}
	}
}
