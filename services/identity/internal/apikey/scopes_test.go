package apikey

import (
	"testing"
)

func TestNormalizeScopesUnrestricted(t *testing.T) {
	got, err := NormalizeScopes(nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil (unrestricted), got %#v", got)
	}
}

func TestNormalizeScopesEmptyGrantsNothing(t *testing.T) {
	in := []string{}
	got, err := NormalizeScopes(&in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil {
		t.Fatal("empty array must not be unrestricted")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %#v", got)
	}
}

func TestNormalizeScopesOk(t *testing.T) {
	in := []string{" widgets:read ", "invoices.write", "a", "b_c", "d-e", "f.g"}
	got, err := NormalizeScopes(&in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d labels: %#v", len(got), got)
	}
}

func TestNormalizeScopesRejects(t *testing.T) {
	cases := []struct {
		in   []string
		want *ScopeError
	}{
		{[]string{"Widgets:read"}, ErrScopeInvalid},
		{[]string{"org/read"}, ErrScopeInvalid},
		{[]string{""}, ErrScopeInvalid},
		{[]string{"widgets:read", "widgets:read"}, ErrScopeDuplicate},
		{[]string{stringsRepeat("a", MaxScopeLen+1)}, ErrScopeTooLong},
	}
	for _, tc := range cases {
		in := tc.in
		_, err := NormalizeScopes(&in)
		if err != tc.want {
			t.Errorf("%v: got %v want %v", tc.in, err, tc.want)
		}
	}

	tooMany := make([]string, MaxScopeCount+1)
	for i := range tooMany {
		tooMany[i] = "s" + itoa(i)
	}
	_, err := NormalizeScopes(&tooMany)
	if err != ErrScopeTooMany {
		t.Errorf("too many: got %v", err)
	}
}

func TestWireScopes(t *testing.T) {
	if WireScopes(nil) != nil {
		t.Fatal("nil must wire as null")
	}
	empty := []string{}
	p := WireScopes(empty)
	if p == nil || len(*p) != 0 {
		t.Fatalf("empty must wire as empty array, got %#v", p)
	}
}

func stringsRepeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
