package apikey

import (
	"errors"
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

func TestConstrainScopes(t *testing.T) {
	t.Run("unrestricted keeps the request", func(t *testing.T) {
		if got, err := ConstrainScopes(nil, nil); err != nil || got != nil {
			t.Fatalf("nil request: got %#v err %v", got, err)
		}
		empty := []string{}
		got, err := ConstrainScopes(nil, empty)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("empty request: got %#v err %v", got, err)
		}
		got, err = ConstrainScopes(nil, []string{"admin"})
		if err != nil || len(got) != 1 || got[0] != "admin" {
			t.Fatalf("list request: got %#v err %v", got, err)
		}
	})

	t.Run("restricted inherit is never null", func(t *testing.T) {
		caller := []string{"projects:read", "projects:write"}
		got, err := ConstrainScopes(caller, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || len(got) != 2 || got[0] != "projects:read" || got[1] != "projects:write" {
			t.Fatalf("inherit: %#v", got)
		}
		caller[0] = "mutated"
		if got[0] != "projects:read" {
			t.Fatal("inherit must copy")
		}

		none := []string{}
		got, err = ConstrainScopes(none, nil)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("empty inherit: %#v err %v", got, err)
		}
	})

	t.Run("subset keeps the request", func(t *testing.T) {
		caller := []string{"projects:read", "projects:write"}
		got, err := ConstrainScopes(caller, []string{"projects:write"})
		if err != nil || len(got) != 1 || got[0] != "projects:write" {
			t.Fatalf("subset: %#v err %v", got, err)
		}
		got, err = ConstrainScopes(caller, []string{})
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("explicit empty is a subset: %#v err %v", got, err)
		}
	})

	t.Run("missing labels", func(t *testing.T) {
		caller := []string{"projects:read"}
		_, err := ConstrainScopes(caller, []string{"projects:read", "admin", "billing:write"})
		var insufficient *InsufficientScopeError
		if !errors.As(err, &insufficient) {
			t.Fatalf("err: %v", err)
		}
		if len(insufficient.Missing) != 2 || insufficient.Missing[0] != "admin" || insufficient.Missing[1] != "billing:write" {
			t.Fatalf("missing: %#v", insufficient.Missing)
		}

		_, err = ConstrainScopes([]string{}, []string{"admin"})
		if !errors.As(err, &insufficient) || len(insufficient.Missing) != 1 || insufficient.Missing[0] != "admin" {
			t.Fatalf("empty caller: %v", err)
		}
	})
}

func TestParseCallerScopes(t *testing.T) {
	got, err := ParseCallerScopes("[]")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty marker: %#v err %v", got, err)
	}
	got, err = ParseCallerScopes("")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("blank: %#v err %v", got, err)
	}
	got, err = ParseCallerScopes(" projects:read, projects:write ")
	if err != nil || len(got) != 2 || got[0] != "projects:read" || got[1] != "projects:write" {
		t.Fatalf("labels: %#v err %v", got, err)
	}
	for _, raw := range []string{"Admin", "projects:read,projects:read", "a,,b", "[] ,projects:read"} {
		if _, err := ParseCallerScopes(raw); err != ErrCallerScopes {
			t.Errorf("%q: got %v", raw, err)
		}
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
