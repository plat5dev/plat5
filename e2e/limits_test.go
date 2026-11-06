package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// answer is a response as a client sees it, less what differs per request.
type answer struct {
	status  int
	body    string
	headers http.Header
}

// perRequest are the headers that differ on every response.
var perRequest = []string{"X-Request-Id", "Date"}

func (s *stack) answer(who caller, method, path string) answer {
	s.t.Helper()
	req, err := http.NewRequest(method, s.gateway+path, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+string(who))
	resp, err := s.http.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	h := resp.Header.Clone()
	for _, name := range perRequest {
		h.Del(name)
	}
	// The request id is in the body too.
	body := strings.ReplaceAll(string(raw), resp.Header.Get("X-Request-Id"), "<request-id>")
	return answer{status: resp.StatusCode, body: body, headers: h}
}

// same fails unless a and b are one answer. Retry-After counts down with the
// window, so it may differ by the second between two requests; it's compared
// for presence.
func same(t *testing.T, what string, a, b answer) {
	t.Helper()
	if a.status != b.status || a.body != b.body {
		t.Fatalf("%s: %d %s / %d %s", what, a.status, a.body, b.status, b.body)
	}
	names := func(h http.Header) []string {
		var out []string
		for name, v := range h {
			if name == "Retry-After" {
				out = append(out, name)
				continue
			}
			out = append(out, name+": "+strings.Join(v, ","))
		}
		slices.Sort(out)
		return out
	}
	if an, bn := names(a.headers), names(b.headers); !slices.Equal(an, bn) {
		t.Fatalf("%s: headers differ:\n%v\n%v", what, an, bn)
	}
}

// With the IP's failed-auth bucket full, an existing project the caller may
// not read and a project that doesn't exist answer the same: neither the
// status, the body nor a header tells them apart
// (docs/concepts/access.md#how-a-request-is-decided). The stack runs the
// limiter low (e2e/compose.yml), and this test waits its window out at the end.
func TestDeniedAndMissingAnswerAlike(t *testing.T) {
	s := newStack(t)
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := s.person("e2e-limits-owner-" + run)
	member := s.person("e2e-limits-member-" + run)

	var org, project node
	s.call(owner, "POST", "/orgs", map[string]string{"display_name": "E2E limits " + run}, 201, &org)
	orgPath := "/orgs/" + org.Slug
	s.call(owner, "POST", orgPath+"/projects", map[string]string{"display_name": "Web"}, 201, &project)
	var invite struct {
		Token string `json:"token"`
	}
	s.call(owner, "POST", orgPath+"/invites", map[string]string{"role": "member"}, 201, &invite)
	s.call(member, "POST", "/user/invites/redeem", map[string]string{"token": invite.Token}, 200, nil)
	settle()

	// member holds organization:read only: the project is a permission 403.
	forbidden := orgPath + "/projects/" + project.Slug
	missing := orgPath + "/projects/no-such-project"
	f, m := s.answer(member, "GET", forbidden), s.answer(member, "GET", missing)
	if f.status != 403 {
		t.Fatalf("forbidden: %d %s", f.status, f.body)
	}
	same(t, "403s", f, m)

	// Fill the IP's bucket with missing slugs.
	var limited answer
	for i := range 1000 {
		a := s.answer(member, "GET", fmt.Sprintf("%s/projects/missing-%d", orgPath, i))
		if a.status == 429 {
			limited = a
			break
		}
		if a.status != 403 {
			t.Fatalf("filling: %d %s", a.status, a.body)
		}
	}
	if limited.status != 429 {
		t.Fatal("the failed-auth limiter never answered")
	}
	f, m = s.answer(member, "GET", forbidden), s.answer(member, "GET", missing)
	if f.status != 429 {
		t.Fatalf("forbidden with the bucket full: %d %s", f.status, f.body)
	}
	same(t, "429s", f, m)
	for name := range f.headers {
		if strings.HasPrefix(name, "X-Ratelimit-") {
			t.Fatalf("a failed-auth 429 carries %s", name)
		}
	}

	// Wait the window out, so the tests after this one start with an empty bucket.
	wait, err := strconv.Atoi(f.headers.Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After %q", f.headers.Get("Retry-After"))
	}
	time.Sleep(time.Duration(wait+1) * time.Second)
	if a := s.answer(member, "GET", missing); a.status != 403 {
		t.Fatalf("after the window: %d %s", a.status, a.body)
	}
}

// A body sent without Content-Type: application/json is a 400, through the
// gateway as on identity's own port.
func TestBodyWithoutJSONContentTypeIs400(t *testing.T) {
	s := newStack(t)
	owner := s.person(fmt.Sprintf("e2e-content-type-%d", time.Now().UnixNano()))
	for _, contentType := range []string{"", "text/plain"} {
		req, err := http.NewRequest("POST", s.gateway+"/orgs", bytes.NewReader([]byte(`{"display_name": "No type"}`)))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("Authorization", "Bearer "+string(owner))
		resp, err := s.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 || !strings.Contains(string(raw), `"INVALID_REQUEST"`) {
			t.Fatalf("Content-Type %q: %d %s", contentType, resp.StatusCode, raw)
		}
	}
}
