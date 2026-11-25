package access

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/plat5dev/plat5/identity/feed"
)

func TestNeedsExternalModes(t *testing.T) {
	cases := []struct {
		mode    string
		builtin bool
		want    bool
	}{
		{feed.AuthorizerModeReplace, true, true},
		{feed.AuthorizerModeReplace, false, true},
		{feed.AuthorizerModeAnd, true, true},
		{feed.AuthorizerModeAnd, false, false},
		{feed.AuthorizerModeOr, true, false},
		{feed.AuthorizerModeOr, false, true},
	}
	for _, tc := range cases {
		if got := NeedsExternal(tc.mode, tc.builtin); got != tc.want {
			t.Errorf("NeedsExternal(%q, %v) = %v, want %v", tc.mode, tc.builtin, got, tc.want)
		}
	}
}

func TestFeedParsesAuthorizersAndTokenEnv(t *testing.T) {
	t.Setenv("TEST_TOPAZ_TOKEN", "tok-abc")
	raw := []byte(`{
		"revision": 3,
		"deletion_grace": "30d",
		"reserved_slugs": [],
		"authorizers": {
			"topaz": {
				"url": "http://topaz:8383/",
				"timeout": "3s",
				"cache_ttl": "0s",
				"negative_cache_ttl": "5s",
				"token_env": "TEST_TOPAZ_TOKEN"
			}
		},
		"kinds": {
			"organization": { "list_permission": "organization:read" },
			"project": {
				"parent": "organization",
				"list_permission": "project:read",
				"authorizer": { "name": "topaz", "mode": "or" }
			}
		},
		"roles": { "admin": { "permissions": ["*"], "bindable_at": ["organization", "project"] } },
		"creator_role": "admin"
	}`)
	snap, err := feed.Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	authz, ok := snap.Authorizers["topaz"]
	if !ok {
		t.Fatal("missing topaz authorizer")
	}
	if authz.URL != "http://topaz:8383" || authz.Timeout != 3*time.Second || authz.Token != "tok-abc" {
		t.Fatalf("unexpected authorizer: %+v", authz)
	}
	proj, _ := snap.Kind("project")
	if proj.Authorizer == nil || proj.Authorizer.Name != "topaz" || proj.Authorizer.Mode != feed.AuthorizerModeOr {
		t.Fatalf("unexpected project authorizer: %+v", proj.Authorizer)
	}

	// Missing env var refuses the snapshot.
	t.Setenv("TEST_TOPAZ_TOKEN", "")
	if _, err := feed.Parse(raw); err == nil {
		t.Fatal("expected error when token_env is unset")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPPDPWireAndFailClosed(t *testing.T) {
	var seenPath, seenAuth string
	pdp := &HTTPPDP{
		client: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				seenPath = r.URL.Path
				seenAuth = r.Header.Get("Authorization")
				rec := httptest.NewRecorder()
				switch r.URL.Path {
				case "/access/v1/evaluation":
					rec.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(rec).Encode(map[string]any{"decision": true})
				case "/access/v1/evaluations":
					var body authzenBatchBody
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						rec.WriteHeader(http.StatusBadRequest)
						return rec.Result(), nil
					}
					evals := make([]map[string]bool, len(body.Evaluations))
					for i, item := range body.Evaluations {
						evals[i] = map[string]bool{"decision": item.Resource.ID == "01p1"}
					}
					rec.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(rec).Encode(map[string]any{"evaluations": evals})
				default:
					rec.WriteHeader(http.StatusInternalServerError)
				}
				return rec.Result(), nil
			}),
		},
	}

	authz := feed.Authorizer{Name: "topaz", URL: "http://topaz.internal:8383", Timeout: time.Second, Token: "secret"}

	// Single evaluation hits /access/v1/evaluation.
	res, err := pdp.EvaluateBatch(context.Background(), authz, []AuthzenRequest{
		{
			Subject:  AuthzenSubject{Type: "member", ID: "01m1"},
			Action:   AuthzenAction{Name: "project:read"},
			Resource: AuthzenResource{Type: "project", ID: "01p1"},
		},
	})
	if err != nil || len(res) != 1 || !res[0] {
		t.Fatalf("single eval got (%v, %v)", res, err)
	}
	if seenPath != "/access/v1/evaluation" || seenAuth != "Bearer secret" {
		t.Fatalf("unexpected request: path=%q auth=%q", seenPath, seenAuth)
	}

	// Multi-item batch hits /access/v1/evaluations.
	res, err = pdp.EvaluateBatch(context.Background(), authz, []AuthzenRequest{
		{Subject: AuthzenSubject{Type: "member", ID: "01m1"}, Action: AuthzenAction{Name: "project:read"}, Resource: AuthzenResource{Type: "project", ID: "01p1"}},
		{Subject: AuthzenSubject{Type: "member", ID: "01m1"}, Action: AuthzenAction{Name: "project:read"}, Resource: AuthzenResource{Type: "project", ID: "01p2"}},
	})
	if err != nil || len(res) != 2 || !res[0] || res[1] {
		t.Fatalf("batch eval got (%v, %v)", res, err)
	}
	if seenPath != "/access/v1/evaluations" {
		t.Fatalf("expected /access/v1/evaluations, got %q", seenPath)
	}

	// Non-2xx fails closed with ErrPDPUnavailable.
	badPDP := &HTTPPDP{
		client: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				rec.WriteHeader(http.StatusBadGateway)
				return rec.Result(), nil
			}),
		},
	}
	if _, err := badPDP.EvaluateBatch(context.Background(), authz, []AuthzenRequest{{}}); !errors.Is(err, ErrPDPUnavailable) {
		t.Fatalf("expected ErrPDPUnavailable, got %v", err)
	}

	// Batch evaluation with omitted decision fails closed with ErrPDPUnavailable.
	missingDecisionPDP := &HTTPPDP{
		client: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				rec.WriteHeader(http.StatusOK)
				_, _ = rec.WriteString(`{"evaluations":[{"decision":true},{}]}`)
				return rec.Result(), nil
			}),
		},
	}
	if _, err := missingDecisionPDP.EvaluateBatch(context.Background(), authz, []AuthzenRequest{{}, {}}); !errors.Is(err, ErrPDPUnavailable) {
		t.Fatalf("expected ErrPDPUnavailable on missing batch decision, got %v", err)
	}
}
