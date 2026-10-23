package e2e

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const keyID = "plat5-e2e"

// stack is a running Plat5 behind its gateway, and an IdP these tests play.
type stack struct {
	t       *testing.T
	gateway string
	issuer  string
	key     *rsa.PrivateKey
	http    *http.Client
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// newStack serves JWKS and waits for the gateway to answer with it loaded.
func newStack(t *testing.T) *stack {
	t.Helper()
	if os.Getenv("PLAT5_E2E") != "1" {
		t.Skip("set PLAT5_E2E=1 to run against a running stack (see README.md)")
	}
	pemBytes, err := os.ReadFile("testdata/jwt-signing-key.pem")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(pemBytes)
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	s := &stack{
		t:       t,
		gateway: strings.TrimRight(env("GATEWAY_URL", "http://localhost:5400"), "/"),
		issuer:  env("AUTH_ISSUER", "http://localhost:5401"),
		key:     parsed.(*rsa.PrivateKey),
		http:    &http.Client{Timeout: 10 * time.Second},
	}
	s.serveJWKS(env("JWKS_ADDR", ":5401"))
	s.waitForGateway()
	return s
}

func (s *stack) serveJWKS(addr string) {
	pub := s.key.PublicKey
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": keyID, "alg": "RS256", "use": "sig",
		"n": b64(pub.N.Bytes()),
		"e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}}})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.t.Fatalf("serve JWKS on %s: %v (is something else on that port?)", addr, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	})}
	go func() { _ = srv.Serve(ln) }()
	s.t.Cleanup(func() { _ = srv.Close() })
}

// waitForGateway polls GET /me with a token until the gateway has routes and JWKS.
func (s *stack) waitForGateway() {
	wait, err := time.ParseDuration(env("E2E_WAIT", "10m"))
	if err != nil {
		s.t.Fatal(err)
	}
	probe := s.person("probe")
	deadline := time.Now().Add(wait)
	last := "no answer"
	for time.Now().Before(deadline) {
		status, body, err := s.try(probe, "GET", "/me", nil)
		if err == nil && status == http.StatusOK {
			return
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("%d %s", status, body)
		}
		time.Sleep(2 * time.Second)
	}
	s.t.Fatalf("gateway at %s not ready after %s: %s", s.gateway, wait, last)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// caller is how a request authenticates: a JWT or an API key.
type caller struct {
	header, value string
}

// person is a JWT for an IdP subject, as Plat5 Auth issues them
// (`properties.user_id`, audience `plat5`).
func (s *stack) person(subject string) caller {
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": keyID})
	claims, _ := json.Marshal(map[string]any{
		"iss":        s.issuer,
		"aud":        "plat5",
		"sub":        subject,
		"exp":        time.Now().Add(time.Hour).Unix(),
		"name":       "E2E " + subject,
		"properties": map[string]string{"user_id": subject},
	})
	signing := b64(head) + "." + b64(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		s.t.Fatal(err)
	}
	return caller{"Authorization", "Bearer " + signing + "." + b64(sig)}
}

func apiKey(key string) caller { return caller{"X-API-Key", key} }

func (s *stack) try(who caller, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.gateway+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(who.header, who.value)
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}

// call sends a request and requires `want`. It decodes a JSON body into `into`.
func (s *stack) call(who caller, method, path string, body any, want int, into any) []byte {
	s.t.Helper()
	status, out, err := s.try(who, method, path, body)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	if status != want {
		s.t.Fatalf("%s %s: got %d, want %d: %s", method, path, status, want, out)
	}
	if into != nil {
		if err := json.Unmarshal(out, into); err != nil {
			s.t.Fatalf("%s %s: %v: %s", method, path, err, out)
		}
	}
	return out
}

type node struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Slug   string `json:"slug"`
	Status string `json:"status"`
}

type apiError struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// The flow e10s needs: an org and a project, a service account at the project,
// a route called through the gateway as it, and the project deleted and restored.
func TestOrgProjectServiceAccountDeleteRestore(t *testing.T) {
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := s.person("e2e-owner-" + run)
	outsider := s.person("e2e-outsider-" + run)

	// A person creates an org, and a project in it.
	var org, project node
	s.call(owner, "POST", "/orgs", map[string]string{"label": "E2E " + run}, 201, &org)
	if org.Kind != "organization" || !strings.HasPrefix(org.ID, "n_") || org.Slug == "" {
		t.Fatalf("org: %+v", org)
	}
	s.call(owner, "POST", "/orgs/"+org.Slug+"/projects", map[string]string{"label": "Web"}, 201, &project)
	projectPath := "/orgs/" + org.Slug + "/projects/" + project.Slug
	byID := "/orgs/" + org.ID + "/projects/" + project.ID

	// A service account at the project, with a key.
	var sa struct {
		ID     string `json:"id"`
		NodeID string `json:"node_id"`
	}
	s.call(owner, "POST", projectPath+"/service-accounts",
		map[string]string{"name": "ci", "role": "developer"}, 201, &sa)
	if sa.NodeID != project.ID || !strings.HasPrefix(sa.ID, "m_") {
		t.Fatalf("service account: %+v", sa)
	}
	var key struct {
		Key string `json:"key"`
	}
	s.call(owner, "POST", projectPath+"/service-accounts/"+sa.ID+"/keys", map[string]string{}, 201, &key)
	bot := apiKey(key.Key)

	// Through the gateway as the service account: allowed by slug and by id…
	var got node
	s.call(bot, "GET", projectPath, nil, 200, &got)
	if got.ID != project.ID {
		t.Fatalf("project by slug: %+v", got)
	}
	s.call(bot, "GET", byID, nil, 200, &got)

	// …refused by label: developer holds no project:delete.
	var denied apiError
	s.call(bot, "DELETE", projectPath, nil, 403, &denied)
	if denied.Error.Message != "Permission denied, or it may not exist." {
		t.Fatalf("label 403: %+v", denied)
	}
	if fmt.Sprint(denied.Error.Details["required_labels"]) != "[project:delete]" {
		t.Fatalf("label 403 details: %+v", denied.Error.Details)
	}

	// Someone outside the org gets the same 403, and so does a project that isn't there.
	var outside, missing apiError
	s.call(outsider, "GET", projectPath, nil, 403, &outside)
	s.call(owner, "GET", "/orgs/"+org.Slug+"/projects/no-such-project", nil, 403, &missing)
	if outside.Error.Message != denied.Error.Message || missing.Error.Message != denied.Error.Message {
		t.Fatalf("403s differ: %+v / %+v / %+v", outside, missing, denied)
	}

	// The org's project list, filtered to what its owner can read.
	var list struct {
		Nodes []node `json:"nodes"`
	}
	s.call(owner, "GET", "/orgs/"+org.Slug+"/projects", nil, 200, &list)
	if len(list.Nodes) != 1 || list.Nodes[0].ID != project.ID {
		t.Fatalf("projects: %+v", list)
	}

	// Delete the project.
	s.call(owner, "DELETE", projectPath, nil, 204, nil)
	time.Sleep(1500 * time.Millisecond) // APIKEY_CACHE_TTL_SECS=1

	// The deleted project is a 403 for its owner, and listed as deleted.
	s.call(owner, "GET", projectPath, nil, 403, nil)
	s.call(owner, "GET", "/orgs/"+org.Slug+"/projects?state=deleted", nil, 200, &list)
	if len(list.Nodes) != 1 || list.Nodes[0].ID != project.ID || list.Nodes[0].Status != "deleted" {
		t.Fatalf("deleted projects: %+v", list)
	}
	// The service account's key was revoked with it, and its born node is gone: 401.
	s.call(bot, "GET", projectPath, nil, 401, nil)

	// Restore it.
	var restored node
	s.call(owner, "POST", projectPath+"/restore", nil, 200, &restored)
	if restored.ID != project.ID || restored.Status != "active" {
		t.Fatalf("restored: %+v", restored)
	}
	time.Sleep(1500 * time.Millisecond)
	s.call(owner, "GET", projectPath, nil, 200, &got)

	// The old key stays revoked; a new one works.
	s.call(bot, "GET", projectPath, nil, 401, nil)
	s.call(owner, "POST", projectPath+"/service-accounts/"+sa.ID+"/keys", map[string]string{}, 201, &key)
	s.call(apiKey(key.Key), "GET", projectPath, nil, 200, &got)
	if got.ID != project.ID {
		t.Fatalf("project after restore: %+v", got)
	}
}
