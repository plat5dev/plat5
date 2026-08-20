package config

import "testing"

func TestParseAPIKeyBrand(t *testing.T) {
	ok := []string{"plat5", "acme", "a", "a1", "happ", "sk"}
	for _, in := range ok {
		got, err := parseAPIKeyBrand(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != in {
			t.Fatalf("%q: got %q", in, got)
		}
	}
	got, err := parseAPIKeyBrand("  acme  ")
	if err != nil || got != "acme" {
		t.Fatalf("trim: %q %v", got, err)
	}

	bad := []string{"", "   ", "Plat5", "acme-app", "1acme", "-x"}
	for _, in := range bad {
		if _, err := parseAPIKeyBrand(in); err == nil {
			t.Fatalf("%q: expected error", in)
		}
	}

	long := make([]byte, maxAPIKeyBrandLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := parseAPIKeyBrand(string(long)); err == nil {
		t.Fatal("expected length error")
	}

	max := make([]byte, maxAPIKeyBrandLen)
	for i := range max {
		max[i] = 'a'
	}
	if _, err := parseAPIKeyBrand(string(max)); err != nil {
		t.Fatalf("max length: %v", err)
	}
}

func TestWirePrefixes(t *testing.T) {
	if g := userAPIKeyPrefix("plat5"); g != "plat5-sk-1-" {
		t.Fatalf("user: %q", g)
	}
	if g := memberAPIKeyPrefix("acme"); g != "acme-mk-1-" {
		t.Fatalf("member: %q", g)
	}
}
