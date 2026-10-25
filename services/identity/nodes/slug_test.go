package nodes

import (
	"strings"
	"testing"
)

func TestValidSlug(t *testing.T) {
	for _, ok := range []string{"abc", "gruow", "a-b", "a1", "a" + strings.Repeat("b", 62), "project-42", "a--b"} {
		if len(ok) >= 3 && !ValidSlug(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "ab", "1abc", "-abc", "abc-", "Abc", "a_b", "n_01J0000000000000000000000", "01jbx9q3t5vk8m2n4p6r7s8t9w", "a" + strings.Repeat("b", 63), "ab c", "café"} {
		if ValidSlug(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
}

func TestBaseSlug(t *testing.T) {
	for _, tc := range []struct{ name, kind, want string }{
		{"Gruow", "organization", "gruow"},
		{"  Acme, Inc.  ", "organization", "acme-inc"},
		{"Gather Room!!", "project", "gather-room"},
		{"42", "project", "project-42"},
		{"3D Labs", "project", "project-3d-labs"},
		{"AB", "project", "project-ab"},
		{"日本", "project", "project"},
		{"", "service_area", "service-area"},
		{"x", "x", "x-x"},
		{"", "x", "x00"},
		{strings.Repeat("abc ", 30), "project", strings.TrimRight(strings.Repeat("abc-", 15), "-")},
	} {
		got := baseSlug(tc.name, tc.kind)
		if got != tc.want {
			t.Fatalf("baseSlug(%q, %q) = %q, want %q", tc.name, tc.kind, got, tc.want)
		}
		if !ValidSlug(got) {
			t.Fatalf("baseSlug(%q, %q) = %q is not a valid slug", tc.name, tc.kind, got)
		}
	}
}

func TestCandidates(t *testing.T) {
	if got := candidate("acme", 1); got != "acme" {
		t.Fatal(got)
	}
	if got := candidate("acme", 3); got != "acme-3" {
		t.Fatal(got)
	}
	long := baseSlug(strings.Repeat("a", 80), "project")
	for _, n := range []int{1, 2, 99, 1000, 123456} {
		c := candidate(long, n)
		if !ValidSlug(c) {
			t.Fatalf("candidate %d = %q is not a valid slug", n, c)
		}
	}
}
