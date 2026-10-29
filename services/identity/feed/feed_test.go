package feed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const good = `{
	"revision": 3, "deletion_grace": "12h", "reserved_slugs": ["new"], "users": { "name": { "claims": ["name", "preferred_username"], "sync": "first_sign_in" } },
	"kinds": {
		"organization": { "path": "/orgs/{organization}", "list_permission": "organization:read" },
		"project": { "parent": "organization", "path": "/projects/{project}", "list_permission": "project:read" }
	},
	"roles": {}, "creator_role": "admin"
}`

func TestParse(t *testing.T) {
	s, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if s.Revision != 3 || s.DeletionGrace != 12*time.Hour || !s.ReservedSlugs["new"] || s.CreatorRole != "admin" {
		t.Fatalf("settings: %+v", s)
	}
	o, _ := s.Kind("organization")
	if !o.IsRoot() || o.ListPermission != "organization:read" {
		t.Fatalf("organization: %+v", o)
	}
	p, _ := s.Kind("project")
	if p.Parent != "organization" || p.IsRoot() || p.ListPermission != "project:read" {
		t.Fatalf("project: %+v", p)
	}
}

func TestParseNameSync(t *testing.T) {
	kinds := `"kinds": { "organization": { "list_permission": "organization:read" } }, "roles": {}, "creator_role": "admin"`
	for users, want := range map[string]NameSync{
		``:             FirstSignIn,
		`"users": {},`: FirstSignIn,
		`"users": { "name": { "claims": ["name"] } },`:                      FirstSignIn,
		`"users": { "name": { "sync": "first_sign_in" } },`:                 FirstSignIn,
		`"users": { "name": { "claims": ["n"], "sync": "each_sign_in" } },`: EachSignIn,
	} {
		s, err := Parse([]byte(`{` + users + kinds + `}`))
		if err != nil {
			t.Fatalf("%s: %v", users, err)
		}
		if s.NameSync != want {
			t.Fatalf("%s: %q", users, s.NameSync)
		}
	}
	if _, err := Parse([]byte(`{"users": { "name": { "sync": "always" } },` + kinds + `}`)); err == nil {
		t.Fatal("an unknown sync parsed")
	}
}

func TestParseResolvesIncludes(t *testing.T) {
	s, err := Parse([]byte(`{
		"kinds": { "organization": { "list_permission": "organization:read" }, "project": { "parent": "organization", "list_permission": "project:read" } },
		"roles": {
			"admin": { "permissions": ["*"], "bindable_at": ["organization", "project"] },
			"member": { "permissions": ["organization:read"], "bindable_at": ["organization"] },
			"developer": { "includes": ["member"], "permissions": ["project:write", "project:read"], "bindable_at": ["organization", "project"] },
			"lead": { "includes": ["developer", "admin"], "permissions": [], "bindable_at": ["project"] },
			"loop": { "includes": ["loop"], "permissions": ["a:b"], "bindable_at": [] }
		},
		"creator_role": "admin"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for slug, want := range map[string]string{
		"admin":     "*",
		"developer": "organization:read project:read project:write",
		"lead":      "* organization:read project:read project:write",
		"loop":      "a:b",
	} {
		r, _ := s.Role(slug)
		if got := strings.Join(r.Permissions, " "); got != want {
			t.Fatalf("%s: %q, want %q", slug, got, want)
		}
	}
	// bindable_at stays the role's own.
	if lead, _ := s.Role("lead"); lead.BindableAtKind("organization") || !lead.BindableAtKind("project") {
		t.Fatalf("lead: %+v", lead)
	}
}

func TestParseRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"no list permission": `{"kinds": {"organization": {}}}`,
		"unknown parent":     `{"kinds": {"p": {"parent": "nope", "list_permission": "p:read"}}}`,
		"bad kind name":      `{"kinds": {"Org": {"list_permission": "o:read"}}}`,
		"bad grace":          `{"deletion_grace": "30 days", "kinds": {"o": {"list_permission": "o:read"}}}`,
		"no kinds":           `{"kinds": {}}`,
		"not json":           `{`,
		"parent list":        `{"kinds": {"o": {"list_permission": "o:read"}, "p": {"parent": ["o"], "list_permission": "p:read"}}}`,
		"unknown include":    `{"kinds": {"o": {"list_permission": "o:read"}}, "roles": {"r": {"includes": ["gone"], "permissions": [], "bindable_at": []}}}`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Fatalf("%s: should be refused", name)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 30 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "90s": 90 * time.Second, "5m": 5 * time.Minute, "12h": 12 * time.Hour} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"0d", "1w", "d", "-1h", "1.5h"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Fatalf("%q should be refused", bad)
		}
	}
}

func TestFollowerLoadsAndFollows(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/identity" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Query().Get("after") {
		case "":
			_, _ = w.Write([]byte(good))
		case "3":
			if polls.Add(1) == 1 {
				// A snapshot identity can't use: skipped, cursor moves past it.
				_, _ = w.Write([]byte(`{"revision": 4, "kinds": {}}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "4":
			_, _ = w.Write([]byte(`{"revision": 5, "kinds": {"team": {"list_permission": "team:read"}}}`))
		default:
			time.Sleep(20 * time.Millisecond)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	src := &Source{}
	f := NewFollower(srv.URL+"/", src, zerolog.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if src.Current().Revision != 3 {
		t.Fatalf("loaded %d", src.Current().Revision)
	}
	go f.Follow(ctx)
	for src.Current().Revision != 5 {
		if ctx.Err() != nil {
			t.Fatal("no change seen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := src.Current().Kind("team"); !ok {
		t.Fatalf("followed to %+v", src.Current())
	}
}

func TestLoadWaitsForAConfig(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusNotFound) // before the first apply
			return
		}
		_, _ = w.Write([]byte(good))
	}))
	defer srv.Close()
	src := &Source{}
	if src.Loaded() {
		t.Fatal("loaded before any config")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := NewFollower(srv.URL, src, zerolog.Nop()).Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !src.Loaded() || calls.Load() != 2 {
		t.Fatalf("loaded %v after %d calls", src.Loaded(), calls.Load())
	}
}
