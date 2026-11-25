package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockAuthzenReq struct {
	Subject struct {
		Type       string         `json:"type"`
		ID         string         `json:"id"`
		Properties map[string]any `json:"properties"`
	} `json:"subject"`
	Action struct {
		Name       string         `json:"name"`
		Properties map[string]any `json:"properties"`
	} `json:"action"`
	Resource struct {
		Type       string         `json:"type"`
		ID         string         `json:"id"`
		Properties map[string]any `json:"properties"`
	} `json:"resource"`
	Context map[string]any `json:"context"`
}

type mockPDP struct {
	mu      sync.Mutex
	allowed map[string]bool
	down    atomic.Bool
	calls   atomic.Int64
	url     string
}

func startMockPDP(t *testing.T, dockerHost string) *mockPDP {
	t.Helper()
	p := &mockPDP{allowed: map[string]bool{}}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen mock pdp: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	p.url = fmt.Sprintf("http://%s:%d", dockerHost, port)

	decide := func(r mockAuthzenReq) bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.allowed[pdpKey(r.Subject.ID, r.Action.Name, r.Resource.Type, r.Resource.ID)]
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		if p.down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/access/v1/evaluation":
			var req mockAuthzenReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"decision": decide(req)})
		case "/access/v1/evaluations":
			var batch struct {
				Evaluations []mockAuthzenReq `json:"evaluations"`
			}
			if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			out := make([]map[string]bool, len(batch.Evaluations))
			for i, item := range batch.Evaluations {
				out[i] = map[string]bool{"decision": decide(item)}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"evaluations": out})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return p
}

func pdpKey(subjectID, action, resType, resID string) string {
	return subjectID + "|" + action + "|" + resType + "|" + resID
}

func (p *mockPDP) grant(subjectID, action, resType, resID string, allow bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if allow {
		p.allowed[pdpKey(subjectID, action, resType, resID)] = true
	} else {
		delete(p.allowed, pdpKey(subjectID, action, resType, resID))
	}
}

func startEchoUpstream(t *testing.T, dockerHost string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen echo upstream: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path":      r.URL.Path,
			"member_id": r.Header.Get("X-Plat5-Member-Id"),
		})
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return fmt.Sprintf("http://%s:%d", dockerHost, port)
}

func configWithDocsAuthorizer(base map[string]any, authzURL, upstreamURL, cacheTTL string) map[string]any {
	base["authorizers"] = map[string]any{
		"pdp": map[string]any{
			"url":                authzURL,
			"timeout":            "2s",
			"cache_ttl":          cacheTTL,
			"negative_cache_ttl": cacheTTL,
		},
	}
	kinds := base["kinds"].(map[string]any)
	projKind := kinds["project"].(map[string]any)
	projKind["authorizer"] = map[string]any{"name": "pdp", "mode": "or"}

	services := base["services"].(map[string]any)
	services["docs"] = map[string]any{
		"url": upstreamURL,
		"scopes": map[string]any{
			"project": map[string]any{
				"upstream_prefix": "/projects/{project.id}",
				"routes": []any{
					map[string]any{
						"path": "/docs/{doc_id}",
						"methods": map[string]any{
							"GET": map[string]any{
								"required_permissions": []string{"docs:read"},
								"authorizer": map[string]any{
									"name":     "pdp",
									"mode":     "or",
									"resource": map[string]any{"type": "document", "id": "{doc_id}"},
								},
							},
							"PATCH": map[string]any{
								"required_permissions": []string{"docs:write"},
								"authorizer": map[string]any{
									"name":     "pdp",
									"mode":     "and",
									"resource": map[string]any{"type": "document", "id": "{doc_id}"},
								},
							},
							"DELETE": map[string]any{
								"required_permissions": []string{"docs:delete"},
								"authorizer": map[string]any{
									"name":     "pdp",
									"mode":     "replace",
									"resource": map[string]any{"type": "document", "id": "{doc_id}"},
								},
							},
						},
					},
				},
			},
		},
	}
	return base
}

// TestOutboundAuthZEN exercises replace/and/or composition modes, record-level
// resource templates, POST /access/routes batching, identity list_permission
// filtering, and fail-closed 503 end to end.
func TestOutboundAuthZEN(t *testing.T) {
	s := newStack(t)
	dockerHost := env("E2E_DOCKER_HOST", "host.docker.internal")

	pdp := startMockPDP(t, dockerHost)
	upstream := startEchoUpstream(t, dockerHost)

	base := s.config()
	t.Cleanup(func() { s.apply(s.config()) })
	s.apply(configWithDocsAuthorizer(base, pdp.url, upstream, "0s"))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := s.person("e2e-authz-owner-" + run)
	bob := s.person("e2e-authz-bob-" + run)

	var org, proj1, proj2 node
	s.call(owner, "POST", "/orgs", map[string]string{"display_name": "AuthZEN " + run}, 201, &org)
	orgPath := "/orgs/" + org.Slug
	s.call(owner, "POST", orgPath+"/projects", map[string]string{"display_name": "Alpha"}, 201, &proj1)
	s.call(owner, "POST", orgPath+"/projects", map[string]string{"display_name": "Beta"}, 201, &proj2)

	var invite struct {
		Token string `json:"token"`
	}
	s.call(owner, "POST", orgPath+"/invites", map[string]string{"role": "member"}, 201, &invite)
	var redeemed struct {
		MemberID string `json:"member_id"`
	}
	s.call(bob, "POST", "/user/invites/redeem", map[string]string{"token": invite.Token}, 200, &redeemed)
	bobM := redeemed.MemberID

	var ownerM membership
	s.call(owner, "GET", orgPath+"/membership", nil, 200, &ownerM)

	docPath := orgPath + "/projects/" + proj1.Slug + "/docs/doc-42"

	// 1. `or` mode: owner (admin *) succeeds without PDP grant; bob is denied until PDP grants doc-42.
	s.call(owner, "GET", docPath, nil, 200, nil)
	s.call(bob, "GET", docPath, nil, 403, nil)

	pdp.grant(bobM, "docs:read", "document", "doc-42", true)
	s.call(bob, "GET", docPath, nil, 200, nil)

	// 2. `and` mode: owner has `*`, still denied until PDP also grants docs:write.
	s.call(owner, "PATCH", docPath, map[string]any{}, 403, nil)
	pdp.grant(ownerM.MemberID, "docs:write", "document", "doc-42", true)
	s.call(owner, "PATCH", docPath, map[string]any{}, 200, nil)

	// 3. `replace` mode: even owner (`*`) needs PDP grant for DELETE.
	s.call(owner, "DELETE", docPath, nil, 403, nil)
	pdp.grant(ownerM.MemberID, "docs:delete", "document", "doc-42", true)
	s.call(owner, "DELETE", docPath, nil, 200, nil)

	// 4. `POST /access/routes` reflects external record-level decisions for bob.
	var checksResp struct {
		Checks []struct {
			Allowed bool `json:"allowed"`
		} `json:"checks"`
	}
	s.call(bob, "POST", "/access/routes", map[string]any{
		"checks": []map[string]string{
			{"method": "GET", "path": docPath},
			{"method": "GET", "path": orgPath + "/projects/" + proj1.Slug + "/docs/doc-99"},
			{"method": "DELETE", "path": docPath},
		},
	}, 200, &checksResp)
	if len(checksResp.Checks) != 3 || !checksResp.Checks[0].Allowed || checksResp.Checks[1].Allowed || checksResp.Checks[2].Allowed {
		t.Fatalf("unexpected /access/routes checks: %+v", checksResp.Checks)
	}

	// 5. Identity `list_permission` (`project.authorizer: or`): bob has no built-in project binding,
	//    so PDP granting project:read on proj2 makes only proj2 appear in GET /orgs/{org}/projects.
	pdp.grant(bobM, "project:read", "project", proj2.ID, true)
	var listResp struct {
		Data []node `json:"data"`
	}
	s.call(bob, "GET", orgPath+"/projects", nil, 200, &listResp)
	if len(listResp.Data) != 1 || listResp.Data[0].ID != proj2.ID {
		t.Fatalf("expected only proj2 (%s), got %+v", proj2.ID, listResp.Data)
	}

	// 6. Fail closed 503 when PDP is down.
	pdp.down.Store(true)
	s.call(bob, "GET", docPath, nil, 503, nil)
	pdp.down.Store(false)
}

// TestOutboundAuthZENTopaz verifies real OpenID AuthZEN 1.0 interoperability
// against a running Topaz instance (see e2e/compose.topaz.yml). Skips unless
// TOPAZ_URL is set.
func TestOutboundAuthZENTopaz(t *testing.T) {
	topazInternal := os.Getenv("TOPAZ_URL")
	if topazInternal == "" {
		t.Skip("set TOPAZ_URL=http://topaz:8383 with e2e/compose.topaz.yml to run Topaz interop tests")
	}
	s := newStack(t)
	dockerHost := env("E2E_DOCKER_HOST", "host.docker.internal")
	topazDir := strings.TrimRight(env("TOPAZ_DIRECTORY_URL", "http://localhost:9393"), "/")
	apiKey := os.Getenv("TOPAZ_API_KEY")

	// 1. Upload ReBAC manifest to Topaz directory.
	manifest, err := os.ReadFile("topaz/manifest.yaml")
	if err != nil {
		t.Fatalf("read topaz manifest: %v", err)
	}
	topazDo(t, s.http, http.MethodPost, topazDir+"/api/v3/directory/manifest", apiKey, "application/yaml", manifest)

	upstream := startEchoUpstream(t, dockerHost)
	base := s.config()
	t.Cleanup(func() { s.apply(s.config()) })
	s.apply(configWithDocsAuthorizer(base, topazInternal, upstream, "0s"))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := s.person("e2e-topaz-owner-" + run)
	bob := s.person("e2e-topaz-bob-" + run)

	var org, proj node
	s.call(owner, "POST", "/orgs", map[string]string{"display_name": "Topaz " + run}, 201, &org)
	orgPath := "/orgs/" + org.Slug
	s.call(owner, "POST", orgPath+"/projects", map[string]string{"display_name": "Docs"}, 201, &proj)

	var invite struct {
		Token string `json:"token"`
	}
	s.call(owner, "POST", orgPath+"/invites", map[string]string{"role": "member"}, 201, &invite)
	var redeemed struct {
		MemberID string `json:"member_id"`
	}
	s.call(bob, "POST", "/user/invites/redeem", map[string]string{"token": invite.Token}, 200, &redeemed)
	bobM := redeemed.MemberID

	docID := "doc-" + run
	docPath := orgPath + "/projects/" + proj.Slug + "/docs/" + docID

	// Before seeding Topaz: bob cannot read docID or see proj in list.
	s.call(bob, "GET", docPath, nil, 403, nil)

	// Seed Topaz objects & viewer relation: member:bobM -> viewer -> document:docID and project:proj.ID.
	for _, obj := range []map[string]any{
		{"object": map[string]any{"type": "member", "id": bobM}},
		{"object": map[string]any{"type": "document", "id": docID}},
		{"object": map[string]any{"type": "project", "id": proj.ID}},
	} {
		b, _ := json.Marshal(obj)
		topazDo(t, s.http, http.MethodPost, topazDir+"/api/v3/directory/object", apiKey, "application/json", b)
	}
	for _, rel := range []map[string]any{
		{"relation": map[string]any{"subject_type": "member", "subject_id": bobM, "relation": "viewer", "object_type": "document", "object_id": docID}},
		{"relation": map[string]any{"subject_type": "member", "subject_id": bobM, "relation": "viewer", "object_type": "project", "object_id": proj.ID}},
	} {
		b, _ := json.Marshal(rel)
		topazDo(t, s.http, http.MethodPost, topazDir+"/api/v3/directory/relation", apiKey, "application/json", b)
	}

	// Now Topaz grants bob GET on docID and visibility of proj, while DELETE remains 403.
	s.call(bob, "GET", docPath, nil, 200, nil)
	s.call(bob, "DELETE", docPath, nil, 403, nil)

	var listResp struct {
		Data []node `json:"data"`
	}
	s.call(bob, "GET", orgPath+"/projects", nil, 200, &listResp)
	if len(listResp.Data) != 1 || listResp.Data[0].ID != proj.ID {
		t.Fatalf("expected Topaz-granted project %s, got %+v", proj.ID, listResp.Data)
	}
}

func topazDo(t *testing.T, client *http.Client, method, url, apiKey, contentType string, body []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if apiKey != "" {
		req.Header.Set("Authorization", "basic "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("topaz %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("topaz %s %s: %d %s", method, url, resp.StatusCode, out)
	}
}
