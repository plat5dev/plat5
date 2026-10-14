package roles

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeControlPlane serves /internal/roles from a list of answers, one per call.
type fakeControlPlane struct {
	mu      sync.Mutex
	answers []func(w http.ResponseWriter, r *http.Request)
	calls   []string
	token   string
}

func (f *fakeControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.URL.RawQuery)
	f.token = r.Header.Get(internalTokenHeader)
	var answer func(w http.ResponseWriter, r *http.Request)
	if len(f.answers) > 0 {
		answer, f.answers = f.answers[0], f.answers[1:]
	}
	f.mu.Unlock()
	if answer == nil {
		<-r.Context().Done()
		return
	}
	answer(w, r)
}

func status(code int) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
}

func doc(revision, body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"revision":` + revision + `,"roles":` + body + `}`))
	}
}

const ownerAll = `{"roles":{"owner":["*"]},"creator_role":"owner","default_role":"owner"}`
const ownerX = `{"roles":{"owner":["x"]},"creator_role":"owner","default_role":"owner"}`

func TestFollowerLoadsAfterMissingThenFollows(t *testing.T) {
	cp := &fakeControlPlane{answers: []func(http.ResponseWriter, *http.Request){
		status(http.StatusNotFound),
		doc("3", ownerAll),
		status(http.StatusNoContent),
		doc("4", `{"roles":{}}`),
		doc("5", ownerX),
	}}
	srv := httptest.NewServer(cp)
	defer srv.Close()

	src := On()
	f := NewFollower(srv.URL+"/", "tok", src, zerolog.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if got := src.Set().Grants(strp("owner")); !reflect.DeepEqual(got, []string{"*"}) {
		t.Fatalf("loaded: %#v", got)
	}

	go f.Follow(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !reflect.DeepEqual(src.Set().Grants(strp("owner")), []string{"x"}) {
		if time.Now().After(deadline) {
			t.Fatal("follow never stored revision 5")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	cp.mu.Lock()
	defer cp.mu.Unlock()
	// The refused revision 4 still moves the cursor.
	want := []string{"", "", "after=3", "after=3", "after=4"}
	if !reflect.DeepEqual(cp.calls[:len(want)], want) {
		t.Fatalf("calls: %#v", cp.calls)
	}
	if cp.token != "tok" {
		t.Fatalf("internal token: %q", cp.token)
	}
}

func TestFollowerLoadStopsWithContext(t *testing.T) {
	cp := &fakeControlPlane{answers: []func(http.ResponseWriter, *http.Request){
		status(http.StatusServiceUnavailable),
	}}
	srv := httptest.NewServer(cp)
	defer srv.Close()

	src := On()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := NewFollower(srv.URL, "", src, zerolog.Nop()).Load(ctx); err == nil {
		t.Fatal("load ends with the context")
	}
	if src.Loaded() {
		t.Fatal("nothing was loaded")
	}
}
