package server_test

import (
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xMuramasa/trismegistus/relay/internal/agents"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// fakeAgents is the host helper on a temp Unix socket: one live delegate run, run_issue-9.
type fakeAgents struct {
	sock string
	mu   sync.Mutex
	reqs []string
	body string // GET /agents answers this when set
}

func newFakeAgents(t *testing.T) *fakeAgents {
	f := &fakeAgents{sock: filepath.Join(t.TempDir(), "agents.sock")}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeAgents) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
	body := f.body
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET /agents":
		if body == "" {
			body = `{"agents":[{"id":"run_issue-9","kind":"delegate","title":"issue-9","alive":true,"transcript":true}]}`
		}
		_, _ = w.Write([]byte(body))
	case "GET /agents/run_issue-9/transcript":
		_, _ = w.Write([]byte(`{"steps":[{"line":3,"kind":"text","text":"hi"}],"next":` + r.URL.Query().Get("after") + `,"alive":true}`))
	case "POST /agents/run_issue-9/stop":
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"stopping":true}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no such running agent"}}`))
	}
}

func (f *fakeAgents) requests() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.reqs, "|")
}

func TestAgentsNotConfigured(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	mac := r.pairAs("mac", store.RoleManager)
	check := func(what string) {
		for _, c := range [][2]string{{"GET", "/api/agents"}, {"GET", "/api/agents/run_x/transcript"}, {"POST", "/api/agents/run_x/stop"}} {
			if st, b := r.do(c[0], c[1], mac, nil); st != 503 || errCode(b) != "not_configured" {
				t.Fatalf("%s %v: %d %s", what, c, st, b)
			}
		}
	}
	check("no socket")
	r.srv.SetAgents(agents.New(filepath.Join(t.TempDir(), "missing.sock")))
	check("helper not running")
	if st, _ := r.do("GET", "/api/agents", "", nil); st != 401 {
		t.Fatalf("no token: %d", st)
	}
}

func TestAgents(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	f := newFakeAgents(t)
	r.srv.SetAgents(agents.New(f.sock))
	phone := r.pair("pixel-8")
	mac := r.pairAs("mac", store.RoleManager)

	if st, b := r.do("GET", "/api/agents", phone, nil); st != 200 || !strings.Contains(string(b), `"id":"run_issue-9"`) {
		t.Fatalf("list: %d %s", st, b)
	}
	if st, b := r.do("GET", "/api/agents/run_issue-9/transcript?after=12&x=1", phone, nil); st != 200 ||
		string(b) != `{"steps":[{"line":3,"kind":"text","text":"hi"}],"next":12,"alive":true}` {
		t.Fatalf("transcript: %d %s", st, b)
	}
	if st, b := r.do("GET", "/api/agents/run_issue-9/transcript", phone, nil); st != 200 || !strings.Contains(string(b), `"next":0`) {
		t.Fatalf("transcript from the start: %d %s", st, b)
	}
	before := f.requests()
	for _, c := range []struct {
		method, path, token string
		st                  int
		code                string
	}{
		{"GET", "/api/agents/run_issue-9/transcript?after=-1", phone, 400, "bad_request"},
		{"GET", "/api/agents/run_issue-9/transcript?after=x", phone, 400, "bad_request"},
		{"GET", "/api/agents/run_..%2Fx/transcript", phone, 404, "not_found"},
		{"GET", "/api/agents/bogus/transcript", phone, 404, "not_found"},
		{"POST", "/api/agents/run_issue-9/stop", phone, 403, "forbidden"},
	} {
		if st, b := r.do(c.method, c.path, c.token, nil); st != c.st || errCode(b) != c.code {
			t.Errorf("%s %s: %d %s, want %d %s", c.method, c.path, st, b, c.st, c.code)
		}
	}
	if f.requests() != before {
		t.Fatalf("refused requests reached the helper: %s", f.requests())
	}
	if st, b := r.do("POST", "/api/agents/run_issue-9/stop", mac, nil); st != 202 || string(b) != `{"stopping":true}` {
		t.Fatalf("stop: %d %s", st, b)
	}
	if st, b := r.do("POST", "/api/agents/proc_gone/stop", mac, nil); st != 404 || errCode(b) != "not_found" {
		t.Fatalf("stop unknown: %d %s", st, b)
	}
	want := "GET /agents|GET /agents/run_issue-9/transcript?after=12|GET /agents/run_issue-9/transcript?after=0|" +
		"POST /agents/run_issue-9/stop|POST /agents/proc_gone/stop"
	if got := f.requests(); got != want {
		t.Fatalf("helper requests:\n%s\nwant\n%s", got, want)
	}
	logs := suiteLogs.String()
	if !strings.Contains(logs, `"agent_id":"run_issue-9","device":"mac","outcome":"ok"`) ||
		!strings.Contains(logs, `"agent_id":"proc_gone","device":"mac","outcome":"not_found"`) {
		t.Fatalf("stop not logged")
	}

	f.mu.Lock()
	f.body = "not json"
	f.mu.Unlock()
	if st, b := r.do("GET", "/api/agents", phone, nil); st != 500 || errCode(b) != "internal" {
		t.Fatalf("bad helper answer: %d %s", st, b)
	}
}
