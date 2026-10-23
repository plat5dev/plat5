package feed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const good = `{
	"revision": 3, "deletion_grace": "12h", "reserved_slugs": ["new"],
	"kinds": {
		"directory": { "sign_in": "idp", "name_claim": "name" },
		"organization": { "label": "Organization", "segment": "orgs" },
		"folder": { "parent": "organization" },
		"project": { "label": "Project", "parent": ["organization", "folder"] }
	},
	"roles": {}, "default_role": null, "creator_role": "admin", "services": {}
}`

func TestParse(t *testing.T) {
	s, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if s.Revision != 3 || s.DeletionGrace != 12*time.Hour || !s.ReservedSlugs["new"] {
		t.Fatalf("settings: %+v", s)
	}
	if k := s.SignInKind(); k.Name != "directory" || !k.IsRoot() || k.DisplayLabel() != "directory" {
		t.Fatalf("sign-in: %+v", k)
	}
	p, _ := s.Kind("project")
	if !p.AllowsParent("folder") || !p.AllowsParent("organization") || p.AllowsParent("project") || p.IsRoot() {
		t.Fatalf("project: %+v", p)
	}
	f, _ := s.Kind("folder")
	if !f.AllowsParent("organization") {
		t.Fatalf("folder: %+v", f)
	}
}

func TestParseRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"no sign-in":      `{"kinds": {"organization": {}}}`,
		"two sign-in":     `{"kinds": {"a": {"sign_in": "idp"}, "b": {"sign_in": "idp"}}}`,
		"sign-in parent":  `{"kinds": {"o": {}, "d": {"sign_in": "idp", "parent": "o"}}}`,
		"unknown parent":  `{"kinds": {"d": {"sign_in": "idp"}, "p": {"parent": "nope"}}}`,
		"bad kind name":   `{"kinds": {"d": {"sign_in": "idp"}, "Org": {}}}`,
		"bad grace":       `{"deletion_grace": "30 days", "kinds": {"d": {"sign_in": "idp"}}}`,
		"no kinds":        `{"kinds": {}}`,
		"not json":        `{`,
		"bad parent type": `{"kinds": {"d": {"sign_in": "idp"}, "p": {"parent": 3}}}`,
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
		if r.URL.Path != "/internal/identity" || r.Header.Get("X-Plat5-Internal-Token") != "tok" {
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
			_, _ = w.Write([]byte(`{"revision": 5, "kinds": {"people": {"sign_in": "idp"}}}`))
		default:
			time.Sleep(20 * time.Millisecond)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	src := &Source{}
	f := NewFollower(srv.URL+"/", "tok", src, zerolog.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if src.Current().Revision != 3 {
		t.Fatalf("loaded %d", src.Current().Revision)
	}
	changed := make(chan struct{}, 4)
	f.OnChange = func() { changed <- struct{}{} }
	go f.Follow(ctx)
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("no change seen")
	}
	if got := src.Current(); got.Revision != 5 || got.SignInKind().Name != "people" {
		t.Fatalf("followed to %+v", got)
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
	if err := NewFollower(srv.URL, "", src, zerolog.Nop()).Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !src.Loaded() || calls.Load() != 2 {
		t.Fatalf("loaded %v after %d calls", src.Loaded(), calls.Load())
	}
}
