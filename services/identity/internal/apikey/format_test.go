package apikey

import (
	"strings"
	"testing"
)

func TestPrefix(t *testing.T) {
	if got := Prefix("plat5"); got != "plat5-k-2-" {
		t.Fatalf("key prefix %q", got)
	}
}

func TestParseGenerated(t *testing.T) {
	s, err := Generate(Prefix("plat5"))
	if err != nil {
		t.Fatal(err)
	}
	if prefix, ok := Parse("plat5", s); !ok || prefix != "plat5-k-2-" {
		t.Fatalf("Parse(%q) = %q %v", s, prefix, ok)
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
		"plat5-s-2-" + secret,
		"plat5-k-1-" + secret,
		"plat5-mk-1-" + secret,
		"plat5-ms-1-" + secret,
		"plat5-k-3-" + secret,
		"plat5k2" + secret,
	} {
		if _, ok := Parse("plat5", s); ok {
			t.Fatalf("Parse(%q) should fail", s)
		}
	}
}
