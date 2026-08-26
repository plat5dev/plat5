package apikey

import (
	"strings"
	"testing"

	"github.com/plat5dev/plat5/identity/errors"
)

func TestParseScopesOmittedUnrestricted(t *testing.T) {
	t.Parallel()
	got, err := ParseScopes(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("omitted must be unrestricted nil, got %#v", got)
	}
}

func TestParseScopesEmptyGrantsNothing(t *testing.T) {
	t.Parallel()
	in := []string{}
	got, err := ParseScopes(&in)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("empty must grant nothing, got %#v", got)
	}
}

func TestNormalizeScopesOk(t *testing.T) {
	t.Parallel()
	got, err := NormalizeScopes([]string{" reports.export ", "read", "write.v2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "reports.export" || got[1] != "read" || got[2] != "write.v2" {
		t.Fatalf("got %#v", got)
	}
}

func TestNormalizeScopesRejects(t *testing.T) {
	t.Parallel()
	many := make([]string, MaxScopeCount+1)
	for i := range many {
		many[i] = "s" + itoa(i)
	}
	cases := []struct {
		name string
		in   []string
		msg  string
	}{
		{"empty label", []string{""}, "A scope label is required."},
		{"whitespace label", []string{"  "}, "A scope label is required."},
		{"uppercase", []string{"Read"}, "Scopes can only use lowercase letters, numbers, colons, dots, underscores, and dashes."},
		{"space", []string{"a b"}, "Scopes can only use lowercase letters, numbers, colons, dots, underscores, and dashes."},
		{"too long", []string{strings.Repeat("a", MaxScopeLen+1)}, "A scope label is too long."},
		{"too many", many, "Too many scopes."},
		{"duplicate", []string{"read", "read"}, "Duplicate scope labels are not allowed."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NormalizeScopes(tc.in)
			if err == nil {
				t.Fatal("expected error")
			}
			api, ok := err.(*errors.ApiError)
			if !ok || api.Code != "VALIDATION_ERROR" {
				t.Fatalf("got %#v", err)
			}
			if api.Message != tc.msg {
				t.Fatalf("message=%q want %q", api.Message, tc.msg)
			}
		})
	}
}

func TestPointerForJSON(t *testing.T) {
	t.Parallel()
	if PointerForJSON(nil) != nil {
		t.Fatal("nil must stay unrestricted")
	}
	empty := PointerForJSON([]string{})
	if empty == nil || len(*empty) != 0 {
		t.Fatalf("empty: %#v", empty)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
