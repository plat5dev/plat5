package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// direct calls identity's service API, as an operator plane does: by id, with
// no credential, the front door's own rules being its own.
func (s *stack) direct(method, path string, body any) (int, []byte) {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.identityAPI+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		s.t.Fatalf("identity's service API at %s: %v (run the stack with e2e/compose.yml, see README.md)", s.identityAPI, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (s *stack) directNode(method, path string, body any, want int) node {
	s.t.Helper()
	status, out := s.direct(method, path, body)
	if status != want {
		s.t.Fatalf("%s %s: got %d, want %d: %s", method, path, status, want, out)
	}
	var n node
	if len(out) > 0 {
		if err := json.Unmarshal(out, &n); err != nil {
			s.t.Fatalf("%s %s: %v: %s", method, path, err, out)
		}
	}
	return n
}

// A purged node is read-only through identity's service API too: a write to
// it, or to what hangs off it, is 409 NODE_PURGED; its slug can still be
// released, and a delete still answers 204
// (docs/concepts/lifecycle.md#deleting-restoring-and-purging).
func TestPurgedNodeIsReadOnly(t *testing.T) {
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())

	// A short grace, until identity has it: a delete's purge_after shows it.
	short := s.config()
	short["deletion_grace"] = "1s"
	s.apply(short)
	t.Cleanup(func() { s.apply(s.config()) })

	org := s.directNode("POST", "/roots/organization", map[string]string{"display_name": "E2E purge " + run}, 201)
	project := s.directNode("POST", "/nodes/"+org.ID+"/children/project", map[string]string{"display_name": "Web"}, 201)
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.directNode("DELETE", "/nodes/"+org.ID, nil, 204)
		got := s.directNode("GET", "/nodes/"+org.ID, nil, 200)
		if got.PurgeAfter != nil && got.DeletedAt != nil {
			after, _ := time.Parse(time.RFC3339Nano, *got.PurgeAfter)
			at, _ := time.Parse(time.RFC3339Nano, *got.DeletedAt)
			if after.Sub(at) <= time.Minute {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("identity never took deletion_grace 1s: %+v", got)
		}
		s.directNode("POST", "/nodes/"+org.ID+"/restore", nil, 200)
		time.Sleep(500 * time.Millisecond)
	}
	// The grace is fixed at the delete, so the e2e config can come back now.
	s.apply(s.config())

	// The sweeper runs every minute.
	deadline = time.Now().Add(90 * time.Second)
	for s.directNode("GET", "/nodes/"+project.ID, nil, 200).Status != "purged" {
		if time.Now().After(deadline) {
			t.Fatal("the org was never purged")
		}
		time.Sleep(2 * time.Second)
	}

	for _, w := range []struct {
		method, path string
		body         any
	}{
		{"PATCH", "/nodes/" + org.ID, map[string]string{"display_name": "Renamed"}},
		{"POST", "/nodes/" + org.ID + "/children/project", map[string]string{"display_name": "Another"}},
		{"POST", "/nodes/" + org.ID + "/members", map[string]any{"idp": map[string]string{"issuer": s.issuer, "subject": "e2e-purged-" + run}, "role": "member"}},
		{"POST", "/nodes/" + project.ID + "/bindings", map[string]string{"member_id": org.ID, "role": "viewer"}},
		{"POST", "/nodes/" + project.ID + "/service-accounts", map[string]string{"name": "ci", "role": "viewer"}},
		{"POST", "/nodes/" + project.ID + "/invites", map[string]string{"role": "viewer"}},
	} {
		status, out := s.direct(w.method, w.path, w.body)
		var refused apiError
		_ = json.Unmarshal(out, &refused)
		if status != 409 || refused.Error.Code != "NODE_PURGED" {
			t.Fatalf("%s %s on a purged node: %d %s", w.method, w.path, status, out)
		}
	}
	status, out := s.direct("PATCH", "/nodes/"+project.ID, map[string]string{"display_name": "Renamed"})
	var refused apiError
	if err := json.Unmarshal(out, &refused); err != nil || status != 409 || refused.Error.Message != "This project was permanently deleted." {
		t.Fatalf("rename a purged project: %d %s", status, out)
	}

	// The exceptions: a delete finds nothing left, and the root's slug is released.
	s.directNode("DELETE", "/nodes/"+project.ID, nil, 204)
	released := s.directNode("POST", "/nodes/"+org.ID+"/release-slug", nil, 200)
	if released.Status != "purged" || released.Slug != "" {
		t.Fatalf("release: %+v", released)
	}
}
