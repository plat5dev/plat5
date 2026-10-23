package id

import "testing"

func TestNewIsValid(t *testing.T) {
	n := New(Node)
	if !Valid(Node, n) {
		t.Fatalf("%q should be a node id", n)
	}
	if Valid(Member, n) {
		t.Fatalf("%q is not a member id", n)
	}
}

func TestValidRejects(t *testing.T) {
	for _, s := range []string{"", "n_", "01J0000000000000000000000Z", "n_01j00000000000000000000000", "n_not-a-ulid", "gruow"} {
		if Valid(Node, s) {
			t.Fatalf("%q should not be a node id", s)
		}
	}
}
