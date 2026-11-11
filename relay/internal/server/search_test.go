package server_test

import (
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/agents"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// fakeSearchHelper is the agents helper's GET /search on a temp Unix socket. "hold…" queries wait until their
// request is cancelled or release is closed.
type fakeSearchHelper struct {
	sock    string
	mu      sync.Mutex
	reqs    []string
	held    chan string
	release chan struct{}
}

func newFakeSearchHelper(t *testing.T) *fakeSearchHelper {
	f := &fakeSearchHelper{sock: filepath.Join(t.TempDir(), "agents.sock"), held: make(chan string, 4), release: make(chan struct{})}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeSearchHelper) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasPrefix(q, "hold"):
		f.held <- q
		select {
		case <-r.Context().Done():
			return
		case <-f.release:
		}
		_, _ = w.Write([]byte(`{"results":[],"more":false}`))
	case q == "android":
		_, _ = w.Write([]byte(`{"results":[{"session_id":"s1","message_id":7,"role":"user","timestamp":1794362337.5,"snippet":"…the Android 37 image…","marks":[[5,7]],"session_title":"Android app","session_source":"api"}],"more":true}`))
	case q == "refused":
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"bad_request","message":"q must be 2 to 200 characters"}}`))
	case q == "garbage":
		_, _ = w.Write([]byte(`not json`))
	default:
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"code":"search_failed","message":"timed out"}}`))
	}
}

func (f *fakeSearchHelper) requests() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.reqs, "|")
}

func TestSearch(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	phone := r.pair("pixel-8")
	mac := r.pairAs("mac", store.RoleManager)

	if st, b := r.do("GET", "/api/search?q=android", mac, nil); st != 503 || errCode(b) != "hermes_unavailable" ||
		!strings.Contains(string(b), "search needs the agents helper") {
		t.Fatalf("no helper: %d %s", st, b)
	}
	r.srv.SetAgents(agents.New(filepath.Join(t.TempDir(), "missing.sock")))
	if st, b := r.do("GET", "/api/search?q=android", mac, nil); st != 503 || errCode(b) != "hermes_unavailable" {
		t.Fatalf("helper not running: %d %s", st, b)
	}

	f := newFakeSearchHelper(t)
	r.srv.SetAgents(agents.New(f.sock))
	for _, tok := range []string{phone, mac} {
		if st, b := r.do("GET", "/api/search?q=%20android%20", tok, nil); st != 200 ||
			!strings.Contains(string(b), `"snippet":"…the Android 37 image…","marks":[[5,7]],"session_title":"Android app"`) {
			t.Fatalf("search: %d %s", st, b)
		}
	}
	if st, _ := r.do("GET", "/api/search?q=android&limit=7", phone, nil); st != 200 {
		t.Fatalf("with a limit: %d", st)
	}
	before := f.requests()
	for _, q := range []string{"", "q=", "q=a", "q=%20a%20", "q=" + strings.Repeat("x", 201), "q=ab&limit=0", "q=ab&limit=101", "q=ab&limit=x"} {
		if st, b := r.do("GET", "/api/search?"+q, phone, nil); st != 400 || errCode(b) != "bad_request" {
			t.Errorf("%q: %d %s", q, st, b)
		}
	}
	if st, _ := r.do("GET", "/api/search?q=android", "", nil); st != 401 {
		t.Fatalf("no token: %d", st)
	}
	if f.requests() != before {
		t.Fatalf("refused requests reached the helper: %s", f.requests())
	}
	want := "GET /search?limit=50&q=android|GET /search?limit=50&q=android|GET /search?limit=7&q=android"
	if before != want {
		t.Fatalf("helper requests:\n%s\nwant\n%s", before, want)
	}

	for q, c := range map[string]struct {
		st   int
		code string
	}{
		"refused": {400, "bad_request"},
		"slow":    {502, "hermes_error"},
		"garbage": {502, "hermes_error"},
	} {
		if st, b := r.do("GET", "/api/search?q="+q, phone, nil); st != c.st || errCode(b) != c.code {
			t.Errorf("%s: %d %s", q, st, b)
		}
	}
	if st, b := r.do("GET", "/api/search?q=slow", phone, nil); st != 502 || !strings.Contains(string(b), `"timed out"`) {
		t.Fatalf("helper message: %d %s", st, b)
	}
	if logs := suiteLogs.String(); strings.Contains(logs, "android") || strings.Contains(logs, "garbage") {
		t.Fatal("a query was logged")
	}
}

// TestSearchSupersede: a device's newer search cancels its older one; another device's is left alone.
func TestSearchSupersede(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	f := newFakeSearchHelper(t)
	r.srv.SetAgents(agents.New(f.sock))
	phone := r.pair("pixel-8")
	mac := r.pairAs("mac", store.RoleManager)

	get := func(q, tok string, out chan<- string) {
		req, _ := http.NewRequest("GET", r.http.URL+"/api/search?q="+q, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		out <- resp.Status[:3] + " " + errCode(b)
	}
	waitHeld := func(want string) {
		select {
		case q := <-f.held:
			if q != want {
				t.Fatalf("held %q, want %q", q, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s never reached the helper", want)
		}
	}
	phoneOld, phoneNew, macOne := make(chan string, 1), make(chan string, 1), make(chan string, 1)
	go get("hold1", phone, phoneOld)
	waitHeld("hold1")
	go get("hold2", mac, macOne)
	waitHeld("hold2")
	go get("hold3", phone, phoneNew)
	waitHeld("hold3")

	select {
	case got := <-phoneOld:
		if got != "409 busy" {
			t.Fatalf("superseded search: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the older search was not cancelled")
	}
	select {
	case got := <-macOne:
		t.Fatalf("another device's search ended: %s", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(f.release)
	for name, ch := range map[string]chan string{"newer": phoneNew, "other device": macOne} {
		if got := <-ch; got != "200 " {
			t.Errorf("%s: %s", name, got)
		}
	}
}
