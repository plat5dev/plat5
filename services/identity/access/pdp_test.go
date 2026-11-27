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

func TestFeedParsesAuthorizerAndTokenEnv(t *testing.T) {
	t.Setenv("TEST_TOPAZ_TOKEN", "tok-abc")
	raw := []byte(`{
		"revision": 3,
		"deletion_grace": "30d",
		"reserved_slugs": [],
		"policy_claims": ["amr", "idp_connection"],
		"authorizer": {
			"url": "http://topaz:8383/",
			"timeout": "3s",
			"token_env": "TEST_TOPAZ_TOKEN"
		},
		"kinds": {
			"organization": { "list_permission": "organization:read" },
			"project": {
				"parent": "organization",
				"list_permission": "project:read"
			}
		}
	}`)
	snap, err := feed.Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if !snap.ExternalAuthz() || snap.Authorizer == nil {
		t.Fatal("expected external authorizer mode")
	}
	if snap.Authorizer.URL != "http://topaz:8383" || snap.Authorizer.Timeout != 3*time.Second || snap.Authorizer.Token != "tok-abc" {
		t.Fatalf("unexpected authorizer: %+v", snap.Authorizer)
	}
	if !snap.PolicyClaims["amr"] || !snap.PolicyClaims["idp_connection"] {
		t.Fatalf("unexpected policy_claims: %+v", snap.PolicyClaims)
	}

	// Missing env var refuses the snapshot.
	t.Setenv("TEST_TOPAZ_TOKEN", "")
	if _, err := feed.Parse(raw); err == nil {
		t.Fatal("expected error when token_env is unset")
	}

	// Mutual exclusion: authorizer + roles is refused.
	t.Setenv("TEST_TOPAZ_TOKEN", "tok-abc")
	both := []byte(`{
		"revision": 4,
		"authorizer": { "url": "http://topaz:8383", "timeout": "3s", "token_env": "TEST_TOPAZ_TOKEN" },
		"kinds": { "organization": { "list_permission": "organization:read" } },
		"roles": { "admin": { "permissions": ["*"], "bindable_at": ["organization"] } },
		"creator_role": "admin"
	}`)
	if _, err := feed.Parse(both); err == nil {
		t.Fatal("expected error when both authorizer and roles are set")
	}
}

func TestBuildNodeAuthzenRequest(t *testing.T) {
	req := BuildNodeAuthzenRequest("01m1", "person", "01r1", "project:read", "project", "01p1", []ChainNode{
		{Kind: "organization", ID: "01r1"},
	})
	if req.Subject.Type != "member" || req.Subject.ID != "01m1" {
		t.Fatalf("unexpected subject: %+v", req.Subject)
	}
	if req.Subject.Properties["principal"] != "person" || req.Subject.Properties["root_id"] != "01r1" {
		t.Fatalf("unexpected subject properties: %+v", req.Subject.Properties)
	}
	if req.Action.Name != "project:read" || req.Resource.Type != "project" || req.Resource.ID != "01p1" {
		t.Fatalf("unexpected action/resource: %+v %+v", req.Action, req.Resource)
	}
	if req.Resource.Properties["root_id"] != "01r1" {
		t.Fatalf("unexpected resource properties: %+v", req.Resource.Properties)
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

	authz := feed.Authorizer{URL: "http://topaz.internal:8383", Timeout: time.Second, Token: "secret"}

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

	// Redirects are never followed and fail closed as ErrPDPUnavailable.
	redirectPDP := NewHTTPPDP()
	redirectPDP.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Location", "http://169.254.169.254/latest/meta-data/")
		rec.WriteHeader(http.StatusFound)
		return rec.Result(), nil
	})
	if _, err := redirectPDP.EvaluateBatch(context.Background(), authz, []AuthzenRequest{{}}); !errors.Is(err, ErrPDPUnavailable) {
		t.Fatalf("expected ErrPDPUnavailable on redirect, got %v", err)
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
