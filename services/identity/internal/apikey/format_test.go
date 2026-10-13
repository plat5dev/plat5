package apikey

import (
	"strings"
	"testing"
)

func TestPrefix(t *testing.T) {
	if got := Prefix("plat5", KindKey); got != "plat5-k-2-" {
		t.Fatalf("key prefix %q", got)
	}
	if got := Prefix("acme", KindSession); got != "acme-s-2-" {
		t.Fatalf("session prefix %q", got)
	}
}

func TestParseGenerated(t *testing.T) {
	for _, kind := range []Kind{KindKey, KindSession} {
		prefix := Prefix("plat5", kind)
		s, err := Generate(prefix)
		if err != nil {
			t.Fatal(err)
		}
		gotKind, gotPrefix, ok := Parse("plat5", s)
		if !ok || gotKind != kind || gotPrefix != prefix {
			t.Fatalf("Parse(%q) = %q %q %v", s, gotKind, gotPrefix, ok)
		}
	}
}

func TestParseVersion1(t *testing.T) {
	secret := strings.Repeat("A", 43)
	for letters, kind := range map[string]Kind{"sk": KindKey, "mk": KindKey, "ms": KindSession} {
		s := "plat5-" + letters + "-1-" + secret
		gotKind, gotPrefix, ok := Parse("plat5", s)
		if !ok || gotKind != kind || gotPrefix != "plat5-"+letters+"-1-" {
			t.Fatalf("Parse(%q) = %q %q %v", s, gotKind, gotPrefix, ok)
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
		"plat5-x-2-" + secret,
		"plat5-k-1-" + secret,
		"plat5-sk-2-" + secret,
		"plat5-k-3-" + secret,
		"plat5k2" + secret,
	} {
		if _, _, ok := Parse("plat5", s); ok {
			t.Fatalf("Parse(%q) should fail", s)
		}
	}
}
