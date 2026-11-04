package secret

import (
	"strings"
	"testing"
)

func TestPrefixes(t *testing.T) {
	if got := KeyPrefix("plat5"); got != "plat5-k-2-" {
		t.Fatalf("key prefix %q", got)
	}
	if got := InvitePrefix("acme"); got != "acme-i-1-" {
		t.Fatalf("invite prefix %q", got)
	}
}

func TestParseGenerated(t *testing.T) {
	for _, prefix := range []string{KeyPrefix("plat5"), InvitePrefix("plat5")} {
		s, err := Generate(prefix)
		if err != nil {
			t.Fatal(err)
		}
		if !Parse(prefix, s) {
			t.Fatalf("Parse(%q, %q) = false", prefix, s)
		}
	}
}

func TestParseRejects(t *testing.T) {
	secret := strings.Repeat("A", 43)
	for _, s := range []string{
		"",
		"plat5-k-2-",
		"plat5-k-2-" + secret[:42],
		"plat5-k-2-" + secret + "A",
		"plat5-k-2-" + secret[:42] + "=",
		"acme-k-2-" + secret,
		"plat5-i-1-" + secret,
		"plat5k2" + secret,
	} {
		if Parse(KeyPrefix("plat5"), s) {
			t.Fatalf("Parse(key, %q) should fail", s)
		}
	}
	for _, s := range []string{
		"plat5-k-2-" + secret,
		"acme-i-1-" + secret,
		"plat5-i-1-" + secret[:42],
	} {
		if Parse(InvitePrefix("plat5"), s) {
			t.Fatalf("Parse(invite, %q) should fail", s)
		}
	}
}
