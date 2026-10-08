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
		"empty file":            ``,
		"no roles":              "roles: {}\ncreator_role: a\ndefault_role: a\n",
		"unknown key":           "roles: {a: []}\ncreator_role: a\ndefault_role: a\nextra: 1\n",
		"missing creator":       "roles: {a: []}\ndefault_role: a\n",
		"creator not a role":    "roles: {a: []}\ncreator_role: b\ndefault_role: a\n",
		"missing default":       "roles: {a: []}\ncreator_role: a\n",
		"default not a role":    "roles: {a: []}\ncreator_role: a\ndefault_role: b\n",
		"bad slug":              "roles: {Admin: []}\ncreator_role: Admin\ndefault_role: Admin\n",
		"null labels":           "roles: {a: }\ncreator_role: a\ndefault_role: a\n",
		"star not alone":        "roles: {a: ['*', x]}\ncreator_role: a\ndefault_role: a\n",
		"bad label":             "roles: {a: [Bad]}\ncreator_role: a\ndefault_role: a\n",
		"duplicate label":       "roles: {a: [x, x]}\ncreator_role: a\ndefault_role: a\n",
		"sa default not a role": "roles: {a: []}\ncreator_role: a\ndefault_role: a\nservice_account_default_role: b\n",
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
	if got := s.List(); got == nil || len(got) != 0 {
		t.Fatalf("list is empty, got %#v", got)
	}
}

func TestGrants(t *testing.T) {
	s := mustParse(t, starter)
	cases := []struct {
		name string
		role *string
		want []string
	}{
		{"wildcard role", strp("owner"), nil},
		{"null role", nil, nil},
		{"label role", strp("admin"), []string{"org:write", "org:members:write", "org:service-accounts:write"}},
		{"empty role", strp("member"), []string{}},
		{"slug removed from file", strp("gone"), []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Grants(tc.role)
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

func TestServiceAccountDefault(t *testing.T) {
	s := mustParse(t, starter)
	if got := *s.ServiceAccountDefault(); got != "member" {
		t.Fatalf("unset falls back to default_role, got %q", got)
	}

	s = mustParse(t, starter+"service_account_default_role: admin\n")
	got, err := s.ChooseServiceAccount(nil)
	if err != nil || got == nil || *got != "admin" {
		t.Fatalf("omitted is service_account_default_role, got %v %v", got, err)
	}
	got, err = s.ChooseServiceAccount(strp("owner"))
	if err != nil || *got != "owner" {
		t.Fatalf("explicit role wins, got %v %v", got, err)
	}
	if got, _ := s.Choose(nil); *got != "member" {
		t.Fatalf("people still get default_role, got %q", *got)
	}
	assertField(t, func() error { _, err := s.ChooseServiceAccount(strp("nope")); return err }(), "That role doesn't exist.")

	var none *Set
	if none.ServiceAccountDefault() != nil {
		t.Fatal("no roles file: no service-account default")
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

func assertField(t *testing.T, err error, message string) {
	t.Helper()
	var apiErr *errors.ApiError
	if !stderrors.As(err, &apiErr) || apiErr.Code != "VALIDATION_ERROR" || apiErr.Message != message {
		t.Fatalf("want 422 %q, got %v", message, err)
	}
}
