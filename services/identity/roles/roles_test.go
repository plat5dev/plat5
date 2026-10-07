package roles

import (
	stderrors "errors"
	"reflect"
	"strings"
	"testing"

	"github.com/plat5dev/plat5/identity/errors"
)

const starter = `
roles:
  owner: ["*"]
  admin: [org:write, org:members:write, org:service-accounts:write]
  member: []
creator_role: owner
default_role: member
`

func mustParse(t *testing.T, src string) *Set {
	t.Helper()
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return s
}

func strp(s string) *string { return &s }

func TestParseStarter(t *testing.T) {
	s := mustParse(t, starter)
	if *s.Creator() != "owner" || *s.Default() != "member" {
		t.Fatalf("creator=%v default=%v", *s.Creator(), *s.Default())
	}
	if s.Grants(strp("owner")) != nil {
		t.Fatal(`["*"] must grant every label (nil)`)
	}
	member := s.Grants(strp("member"))
	if member == nil || len(member) != 0 {
		t.Fatalf("[] must grant nothing, got %#v", member)
	}
	want := []string{"org:write", "org:members:write", "org:service-accounts:write"}
	if got := s.Grants(strp("admin")); !reflect.DeepEqual(got, want) {
		t.Fatalf("admin=%#v", got)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"empty file":         ``,
		"no roles":           "roles: {}\ncreator_role: a\ndefault_role: a\n",
		"unknown key":        "roles: {a: []}\ncreator_role: a\ndefault_role: a\nextra: 1\n",
		"missing creator":    "roles: {a: []}\ndefault_role: a\n",
		"creator not a role": "roles: {a: []}\ncreator_role: b\ndefault_role: a\n",
		"missing default":    "roles: {a: []}\ncreator_role: a\n",
		"default not a role": "roles: {a: []}\ncreator_role: a\ndefault_role: b\n",
		"bad slug":           "roles: {Admin: []}\ncreator_role: Admin\ndefault_role: Admin\n",
		"null labels":        "roles: {a: }\ncreator_role: a\ndefault_role: a\n",
		"star not alone":     "roles: {a: ['*', x]}\ncreator_role: a\ndefault_role: a\n",
		"bad label":          "roles: {a: [Bad]}\ncreator_role: a\ndefault_role: a\n",
		"duplicate label":    "roles: {a: [x, x]}\ncreator_role: a\ndefault_role: a\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseTooManyLabels(t *testing.T) {
	var b strings.Builder
	b.WriteString("roles:\n  a: [")
	for i := 0; i <= MaxLabels; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("l")
		b.WriteString(strings.Repeat("x", i%10))
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(string(rune('a' + i/26)))
	}
	b.WriteString("]\ncreator_role: a\ndefault_role: a\n")
	if _, err := Parse([]byte(b.String())); err == nil {
		t.Fatalf("expected more than %d labels to be refused", MaxLabels)
	}
}

func TestLoadEmptyPathIsNoRoles(t *testing.T) {
	s, err := Load("")
	if err != nil || s != nil {
		t.Fatalf("got %v, %v", s, err)
	}
}

func TestNilSetIsUnrestricted(t *testing.T) {
	var s *Set
	if s.Creator() != nil || s.Default() != nil {
		t.Fatal("no roles file has no creator or default")
	}
	if s.Grants(strp("anything")) != nil {
		t.Fatal("no roles file grants every label")
	}
	if got := s.Resolve(strp("x"), []string{"a"}); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("resolve passes the credential through, got %#v", got)
	}
	if got := s.List(); got == nil || len(got) != 0 {
		t.Fatalf("list is empty, got %#v", got)
	}
	if err := s.CheckActOn([]string{}, strp("x")); err != nil {
		t.Fatalf("no roles file: no act-on cap, got %v", err)
	}
	if err := s.CheckAssign([]string{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestResolve(t *testing.T) {
	s := mustParse(t, starter)
	cases := []struct {
		name       string
		role       *string
		credential []string
		want       []string
	}{
		{"wildcard role, unrestricted key", strp("owner"), nil, nil},
		{"wildcard role, narrowed key", strp("owner"), []string{"x"}, []string{"x"}},
		{"null role, unrestricted key", nil, nil, nil},
		{"null role, narrowed key", nil, []string{"x"}, []string{"x"}},
		{"label role, unrestricted key", strp("admin"), nil, []string{"org:write", "org:members:write", "org:service-accounts:write"}},
		{"label role, narrowed key", strp("admin"), []string{"org:write", "x"}, []string{"org:write"}},
		{"empty role", strp("member"), nil, []string{}},
		{"slug removed from file", strp("gone"), nil, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Resolve(tc.role, tc.credential)
			if (got == nil) != (tc.want == nil) || !reflect.DeepEqual(append([]string{}, got...), append([]string{}, tc.want...)) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestList(t *testing.T) {
	got := mustParse(t, starter).List()
	if len(got) != 3 || got[0].Slug != "admin" || got[1].Slug != "member" || got[2].Slug != "owner" {
		t.Fatalf("sorted by slug: %#v", got)
	}
	if got[2].Scopes != nil {
		t.Fatal("owner lists as every label (nil)")
	}
}

func TestChoose(t *testing.T) {
	s := mustParse(t, starter)
	got, err := s.Choose(nil)
	if err != nil || got == nil || *got != "member" {
		t.Fatalf("omitted is default_role, got %v %v", got, err)
	}
	got, err = s.Choose(strp(" admin "))
	if err != nil || *got != "admin" {
		t.Fatalf("got %v %v", got, err)
	}
	assertField(t, func() error { _, err := s.Choose(strp("nope")); return err }(), "That role doesn't exist.")

	var none *Set
	got, err = none.Choose(nil)
	if err != nil || got != nil {
		t.Fatalf("no roles file, omitted: NULL role, got %v %v", got, err)
	}
	assertField(t, func() error { _, err := none.Choose(strp("admin")); return err }(), "Roles aren't set up for this deployment.")
}

func TestCheckAssign(t *testing.T) {
	s := mustParse(t, starter)
	admin := s.Grants(strp("admin"))

	if err := s.CheckAssign(nil, strp("owner")); err != nil {
		t.Fatalf("unrestricted caller assigns anything: %v", err)
	}
	if err := s.CheckAssign(admin, strp("admin")); err != nil {
		t.Fatalf("admin assigns admin: %v", err)
	}
	if err := s.CheckAssign(admin, strp("member")); err != nil {
		t.Fatalf("admin assigns member: %v", err)
	}
	assertInsufficient(t, s.CheckAssign(admin, strp("owner")), "You can't assign the owner role.", []string{"*"})
	assertInsufficient(t, s.CheckAssign([]string{"org:write"}, strp("admin")), "You can't assign the admin role.",
		[]string{"org:members:write", "org:service-accounts:write"})
}

func TestCheckActOn(t *testing.T) {
	s := mustParse(t, starter)
	admin := s.Grants(strp("admin"))

	if err := s.CheckActOn(admin, strp("member")); err != nil {
		t.Fatalf("admin acts on member: %v", err)
	}
	if err := s.CheckActOn(admin, strp("gone")); err != nil {
		t.Fatalf("a removed role grants nothing, so anyone may fix it: %v", err)
	}
	assertInsufficient(t, s.CheckActOn(admin, strp("owner")), "You can't change a member with the owner role.", []string{"*"})
	assertInsufficient(t, s.CheckActOn(admin, nil), "You can't change an unrestricted member.", []string{"*"})
	if err := s.CheckActOn(nil, nil); err != nil {
		t.Fatalf("unrestricted caller acts on anyone: %v", err)
	}
}

func assertField(t *testing.T, err error, message string) {
	t.Helper()
	var apiErr *errors.ApiError
	if !stderrors.As(err, &apiErr) || apiErr.Code != "VALIDATION_ERROR" || apiErr.Message != message {
		t.Fatalf("want 422 %q, got %v", message, err)
	}
}

func assertInsufficient(t *testing.T, err error, message string, missing []string) {
	t.Helper()
	var apiErr *errors.ApiError
	if !stderrors.As(err, &apiErr) || apiErr.Code != "INSUFFICIENT_SCOPE" || apiErr.Status != 403 {
		t.Fatalf("want 403 INSUFFICIENT_SCOPE, got %v", err)
	}
	if apiErr.Message != message {
		t.Fatalf("message %q, want %q", apiErr.Message, message)
	}
	details := apiErr.Details.(map[string]any)
	if !reflect.DeepEqual(details["scopes"], missing) {
		t.Fatalf("details.scopes %#v, want %#v", details["scopes"], missing)
	}
}
