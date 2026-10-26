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
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const keyID = "plat5-e2e"

// stack is a running Plat5 behind its gateway, and an IdP these tests play.
type stack struct {
	t            *testing.T
	gateway      string
	controlPlane string
	adminToken   string
	issuer       string
	key          *rsa.PrivateKey
	http         *http.Client
	wait         time.Duration
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// newStack serves JWKS, applies the e2e's config, and waits for the gateway to
// answer with both loaded.
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
	wait, err := time.ParseDuration(env("E2E_WAIT", "10m"))
	if err != nil {
		t.Fatal(err)
	}
	s := &stack{
		t:            t,
		gateway:      strings.TrimRight(env("GATEWAY_URL", "http://localhost:5400"), "/"),
		controlPlane: strings.TrimRight(env("CONTROL_PLANE_URL", "http://localhost:5402"), "/"),
		adminToken:   env("ADMIN_TOKEN", "dev-admin-token"),
		issuer:       env("AUTH_ISSUER", "http://localhost:5401"),
		key:          parsed.(*rsa.PrivateKey),
		http:         &http.Client{Timeout: 10 * time.Second},
		wait:         wait,
	}
	s.serveJWKS(env("JWKS_ADDR", ":5401"))
	s.applyConfig()
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

// applyConfig applies testdata/config.json through the control plane's admin
// API: a dry run for the plan's revision, then the apply with it as `expect`.
// A plan with no changes is skipped, so a rerun applies nothing. With audit
// off, the config leaves the audit service out.
func (s *stack) applyConfig() {
	raw, err := os.ReadFile("testdata/config.json")
	if err != nil {
		s.t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		s.t.Fatal(err)
	}
	if os.Getenv("AUDIT_ENABLED") == "false" {
		delete(config["services"].(map[string]any), "audit")
	}

	var plan struct {
		Revision int   `json:"revision"`
		Changes  []any `json:"changes"`
	}
	deadline := time.Now().Add(s.wait)
	for {
		status, body, err := s.admin("POST", "/apply?dry_run=true", config)
		if err == nil && status == http.StatusOK {
			if err := json.Unmarshal(body, &plan); err != nil {
				s.t.Fatalf("plan: %v: %s", err, body)
			}
			break
		}
		if err == nil && status != http.StatusServiceUnavailable {
			s.t.Fatalf("dry run: %d %s", status, body)
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("control plane at %s not ready after %s: %v %d %s", s.controlPlane, s.wait, err, status, body)
		}
		time.Sleep(2 * time.Second)
	}
	if len(plan.Changes) == 0 {
		return
	}
	config["expect"] = map[string]int{"revision": plan.Revision}
	status, body, err := s.admin("POST", "/apply", config)
	if err != nil || status != http.StatusOK {
		s.t.Fatalf("apply: %v %d %s", err, status, body)
	}
}

func (s *stack) admin(method, path string, body any) (int, []byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, s.controlPlane+path, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.adminToken)
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}

// waitForGateway polls GET /user with a token until the gateway has routes and JWKS.
func (s *stack) waitForGateway() {
	probe := s.person("probe")
	deadline := time.Now().Add(s.wait)
	last := "no answer"
	for time.Now().Before(deadline) {
		status, body, err := s.try(probe, "GET", "/user", nil)
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
	s.t.Fatalf("gateway at %s not ready after %s: %s", s.gateway, s.wait, last)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// caller is a credential, sent as `Authorization: Bearer`: a JWT or an API key.
type caller string

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
	return caller(signing + "." + b64(sig))
}

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
	req.Header.Set("Authorization", "Bearer "+string(who))
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

// settle waits out the gateway's admit cache; the e2e runs it with 1s TTLs.
func settle() { time.Sleep(1500 * time.Millisecond) }

var idShape = regexp.MustCompile(`^[0-7][0-9a-hjkmnp-tv-z]{25}$`)

// isID is a bare lowercase ULID.
func isID(s string) bool { return idShape.MatchString(s) }

type node struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	ParentID    *string `json:"parent_id"`
	RootID      string  `json:"root_id"`
	Slug        string  `json:"slug"`
	DisplayName string  `json:"display_name"`
	Status      string  `json:"status"`
	PurgeAfter  *string `json:"purge_after"`
	Ancestors   []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Slug string `json:"slug"`
	} `json:"ancestors"`
}

type nodeList struct {
	Nodes []node `json:"nodes"`
}

type binding struct {
	ID        string `json:"id"`
	MemberID  string `json:"member_id"`
	NodeID    string `json:"node_id"`
	Role      string `json:"role"`
	Inherited bool   `json:"inherited"`
	Principal string `json:"principal"`
}

type membership struct {
	MemberID    string    `json:"member_id"`
	Bindings    []binding `json:"bindings"`
	Permissions []string  `json:"permissions"`
}

type key struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	KeyPrefix string `json:"key_prefix"`
}

type apiError struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// The flow e10s needs: an org, a project and an environment, a service account
// at the environment, a route called through the gateway as it, who holds what
// there, and the project deleted and restored: a restore undoes the delete,
// keys included.
func TestOrgProjectServiceAccountDeleteRestore(t *testing.T) {
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := s.person("e2e-owner-" + run)
	outsider := s.person("e2e-outsider-" + run)

	// A person creates an org, a project in it, and an environment in that.
	var org, project, env node
	s.call(owner, "POST", "/orgs", map[string]string{"display_name": "E2E " + run}, 201, &org)
	if org.Kind != "organization" || !isID(org.ID) || org.Slug == "" || org.DisplayName != "E2E "+run ||
		org.RootID != org.ID || org.ParentID != nil || len(org.Ancestors) != 0 {
		t.Fatalf("org: %+v", org)
	}
	orgPath := "/orgs/" + org.Slug
	s.call(owner, "POST", orgPath+"/projects", map[string]string{"display_name": "Web"}, 201, &project)
	if !isID(project.ID) || project.ParentID == nil || *project.ParentID != org.ID || project.RootID != org.ID ||
		len(project.Ancestors) != 1 || project.Ancestors[0].ID != org.ID {
		t.Fatalf("project: %+v", project)
	}
	projectPath := orgPath + "/projects/" + project.Slug
	s.call(owner, "POST", projectPath+"/envs", map[string]string{"display_name": "Development"}, 201, &env)
	if env.Kind != "environment" || env.Slug != "development" || env.ParentID == nil || *env.ParentID != project.ID ||
		env.RootID != org.ID || len(env.Ancestors) != 2 || env.Ancestors[1].ID != project.ID {
		t.Fatalf("environment: %+v", env)
	}
	envPath := projectPath + "/envs/" + env.Slug
	byID := "/orgs/" + org.ID + "/projects/" + project.ID + "/envs/" + env.ID

	// Service accounts belong to an org or an environment: the config turns
	// them off at projects, items included, so those routes match nothing.
	// Likewise invites at environments, and new bindings there: people aren't
	// bound at an environment; a service account gets its binding when it's
	// created.
	s.call(owner, "POST", projectPath+"/service-accounts",
		map[string]string{"name": "ci", "role": "developer"}, 404, nil)
	s.call(owner, "GET", projectPath+"/service-accounts/"+org.ID, nil, 404, nil)
	s.call(owner, "GET", envPath+"/invites", nil, 404, nil)
	s.call(owner, "POST", envPath+"/bindings",
		map[string]string{"member_id": org.ID, "role": "developer"}, 404, nil)

	// A service account at the environment, with a key.
	var sa struct {
		ID     string `json:"id"`
		NodeID string `json:"node_id"`
	}
	s.call(owner, "POST", envPath+"/service-accounts",
		map[string]string{"name": "ci", "role": "developer"}, 201, &sa)
	if sa.NodeID != env.ID || !isID(sa.ID) {
		t.Fatalf("service account: %+v", sa)
	}
	var saKey key
	s.call(owner, "POST", envPath+"/service-accounts/"+sa.ID+"/keys", map[string]string{}, 201, &saKey)
	if !isID(saKey.ID) || !strings.HasPrefix(saKey.Key, "plat5-k-2-") {
		t.Fatalf("key: %+v", saKey)
	}
	bot := caller(saKey.Key)

	// Through the gateway as the service account: allowed by slug and by id…
	var got node
	s.call(bot, "GET", envPath, nil, 200, &got)
	if got.ID != env.ID {
		t.Fatalf("environment by slug: %+v", got)
	}
	s.call(bot, "GET", byID, nil, 200, &got)

	// …refused by permission: an environment's routes require project's
	// permissions (permission_name), and developer holds no project:delete.
	var denied apiError
	s.call(bot, "DELETE", envPath, nil, 403, &denied)
	if denied.Error.Message != "Permission denied, or it may not exist." {
		t.Fatalf("permission 403: %+v", denied)
	}
	if fmt.Sprint(denied.Error.Details["required_permissions"]) != "[project:delete]" {
		t.Fatalf("permission 403 details: %+v", denied.Error.Details)
	}
	// …and above its bindings: nothing at the project.
	s.call(bot, "GET", projectPath, nil, 403, nil)

	// Someone outside the org gets the same 403, and so does an environment that isn't there.
	var outside, missing apiError
	s.call(outsider, "GET", envPath, nil, 403, &outside)
	s.call(owner, "GET", projectPath+"/envs/no-such-env", nil, 403, &missing)
	if outside.Error.Message != denied.Error.Message || missing.Error.Message != denied.Error.Message {
		t.Fatalf("403s differ: %+v / %+v / %+v", outside, missing, denied)
	}
	// On an any_member route a 403 only means not in the tree: no permissions named.
	var notMember apiError
	s.call(outsider, "GET", orgPath+"/membership", nil, 403, &notMember)
	if notMember.Error.Message != denied.Error.Message || notMember.Error.Details != nil {
		t.Fatalf("any_member 403: %+v", notMember)
	}

	// What the service account holds at the environment: developer, member included.
	var held membership
	s.call(bot, "GET", envPath+"/membership", nil, 200, &held)
	want := []string{"organization:read", "project:bindings:read", "project:read", "project:write"}
	if held.MemberID != sa.ID || len(held.Bindings) != 1 || held.Bindings[0].Role != "developer" ||
		!slices.Equal(held.Permissions, want) {
		t.Fatalf("service account's membership: %+v", held)
	}
	// The creator holds admin on the org. Projects and environments don't bind
	// their creator in this config: the org's admin reaches them.
	var mine membership
	s.call(owner, "GET", orgPath+"/membership", nil, 200, &mine)
	if !isID(mine.MemberID) || !slices.Equal(mine.Permissions, []string{"*"}) {
		t.Fatalf("owner's membership: %+v", mine)
	}

	// The environment's access list: its own binding, the service account's, and
	// the owner's admin, inherited from the org.
	var access struct {
		Bindings []binding `json:"bindings"`
	}
	s.call(owner, "GET", envPath+"/bindings", nil, 200, &access)
	var own, inherited []binding
	for _, b := range access.Bindings {
		if b.Inherited {
			inherited = append(inherited, b)
		} else {
			own = append(own, b)
		}
	}
	if len(inherited) != 1 || inherited[0].NodeID != org.ID || inherited[0].MemberID != mine.MemberID ||
		inherited[0].Role != "admin" || inherited[0].Principal != "person" {
		t.Fatalf("inherited bindings: %+v", access)
	}
	if len(own) != 1 || own[0].MemberID != sa.ID || own[0].Role != "developer" || own[0].NodeID != env.ID ||
		own[0].Principal != "service_account" {
		t.Fatalf("the environment's own bindings: %+v", access)
	}
	s.call(owner, "GET", envPath+"/bindings?inherited=false", nil, 200, &access)
	if len(access.Bindings) != 1 {
		t.Fatalf("?inherited=false: %+v", access)
	}

	// The org's project list, filtered to what its owner can read.
	var list nodeList
	s.call(owner, "GET", orgPath+"/projects", nil, 200, &list)
	if len(list.Nodes) != 1 || list.Nodes[0].ID != project.ID {
		t.Fatalf("projects: %+v", list)
	}

	// Delete the project: its environment is deleted with it.
	s.call(owner, "DELETE", projectPath, nil, 204, nil)
	settle()

	// The deleted project is a 403 for its owner, and listed as deleted.
	s.call(owner, "GET", projectPath, nil, 403, nil)
	s.call(owner, "GET", orgPath+"/projects?status=deleted", nil, 200, &list)
	if len(list.Nodes) != 1 || list.Nodes[0].ID != project.ID || list.Nodes[0].Status != "deleted" ||
		list.Nodes[0].PurgeAfter == nil {
		t.Fatalf("deleted projects: %+v", list)
	}
	s.call(owner, "GET", orgPath+"/projects", nil, 200, &list)
	if len(list.Nodes) != 0 {
		t.Fatalf("live projects after the delete: %+v", list)
	}
	// The service account's node is deleted, so its key is refused: 401.
	s.call(bot, "GET", envPath, nil, 401, nil)

	// Restore the project, and its environment with it.
	var restored node
	s.call(owner, "POST", projectPath+"/restore", nil, 200, &restored)
	if restored.ID != project.ID || restored.Status != "active" || restored.PurgeAfter != nil {
		t.Fatalf("restored: %+v", restored)
	}
	settle()
	s.call(owner, "GET", envPath, nil, 200, &got)
	// The same key works again: the delete revoked nothing.
	s.call(bot, "GET", envPath, nil, 200, &got)
	if got.ID != env.ID {
		t.Fatalf("environment after restore: %+v", got)
	}
}

// A person's own routes: their account, their trees, a personal key, and an
// invite redeemed by someone else, who then leaves.
func TestUserRoutesKeysInvites(t *testing.T) {
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := s.person("e2e-user-owner-" + run)
	friend := s.person("e2e-user-friend-" + run)

	var me struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	s.call(owner, "GET", "/user", nil, 200, &me)
	if !isID(me.ID) || me.Name != "E2E e2e-user-owner-"+run || me.Status != "active" {
		t.Fatalf("user: %+v", me)
	}

	var org node
	s.call(owner, "POST", "/orgs", map[string]string{"display_name": "People " + run}, 201, &org)
	orgPath := "/orgs/" + org.Slug

	// The org switcher.
	var trees nodeList
	s.call(owner, "GET", "/user/nodes?kind=organization", nil, 200, &trees)
	if len(trees.Nodes) != 1 || trees.Nodes[0].ID != org.ID || len(trees.Nodes[0].Ancestors) != 0 {
		t.Fatalf("user nodes: %+v", trees)
	}

	// A personal key reaches what its person can.
	var personal key
	s.call(owner, "POST", "/user/keys", map[string]string{"name": "laptop"}, 201, &personal)
	if !isID(personal.ID) || !strings.HasPrefix(personal.Key, "plat5-k-2-") {
		t.Fatalf("personal key: %+v", personal)
	}
	var keys struct {
		Keys []key `json:"keys"`
	}
	s.call(owner, "GET", "/user/keys", nil, 200, &keys)
	if len(keys.Keys) != 1 || keys.Keys[0].ID != personal.ID || keys.Keys[0].Key != "" ||
		keys.Keys[0].KeyPrefix != personal.KeyPrefix {
		t.Fatalf("user keys: %+v", keys)
	}
	laptop := caller(personal.Key)
	var got node
	s.call(laptop, "GET", orgPath, nil, 200, &got)
	var meByKey struct {
		ID string `json:"id"`
	}
	s.call(laptop, "GET", "/user", nil, 200, &meByKey)
	if got.ID != org.ID || meByKey.ID != me.ID {
		t.Fatalf("by personal key: %+v %+v", got, meByKey)
	}

	// An invite to the org, redeemed by a friend.
	var invite struct {
		ID    string `json:"id"`
		Role  string `json:"role"`
		Token string `json:"token"`
	}
	s.call(owner, "POST", orgPath+"/invites", map[string]string{"role": "member"}, 201, &invite)
	if !isID(invite.ID) || invite.Role != "member" || !strings.HasPrefix(invite.Token, "inv_") {
		t.Fatalf("invite: %+v", invite)
	}
	s.call(friend, "GET", orgPath, nil, 403, nil)
	var redeemed struct {
		MemberID string `json:"member_id"`
		Node     node   `json:"node"`
	}
	s.call(friend, "POST", "/user/invites/redeem", map[string]string{"token": invite.Token}, 200, &redeemed)
	if !isID(redeemed.MemberID) || redeemed.Node.ID != org.ID || redeemed.Node.Slug != org.Slug {
		t.Fatalf("redeem: %+v", redeemed)
	}
	settle()
	s.call(friend, "GET", orgPath, nil, 200, &got)
	var held membership
	s.call(friend, "GET", orgPath+"/membership", nil, 200, &held)
	if held.MemberID != redeemed.MemberID || !slices.Equal(held.Permissions, []string{"organization:read"}) {
		t.Fatalf("friend's membership: %+v", held)
	}
	s.call(friend, "POST", orgPath+"/projects", map[string]string{"display_name": "No"}, 403, nil)

	// The friend leaves; the owner revokes the personal key.
	s.call(friend, "DELETE", orgPath+"/membership", nil, 204, nil)
	s.call(owner, "DELETE", "/user/keys/"+personal.ID, nil, 204, nil)
	settle()
	s.call(friend, "GET", orgPath, nil, 403, nil)
	s.call(laptop, "GET", orgPath, nil, 401, nil)
}
