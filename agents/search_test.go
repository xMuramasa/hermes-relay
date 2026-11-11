package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuf is a log sink the server's goroutines may write to.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// fakeSearch acts on the query ($4, after -I -c code); "hold" counts its start in $DIR/starts and waits for
// $DIR/release.
func fakeSearch(t *testing.T, e *env) (argvFile, dir string) {
	argvFile = fakeInterpreter(t, e, `case "$4" in
android) echo 'some import chatter'; echo '{"results": [{"session_id": "s1", "message_id": 7}], "more": true}' ;;
penpot) echo 'Traceback (most recent call last):' >&2; echo 'sqlite3.OperationalError: fts5: syntax error near penpot' >&2; exit 1 ;;
slow) exec sleep 5 ;;
garbage) echo 'not json' ;;
nothing) echo '{"more": false}' ;;
hold) echo x >> "$DIR/starts"; while [ ! -e "$DIR/release" ]; do sleep 0.02; done; echo '{"results": [], "more": false}' ;;
esac
`)
	e.h.SearchTimeout = 1500 * time.Millisecond
	return argvFile, filepath.Dir(argvFile)
}

func TestSearch(t *testing.T) {
	e := newEnv(t, "")
	logs := &lockedBuf{}
	e.h.Log = slog.New(slog.NewTextHandler(logs, nil))
	argvFile, _ := fakeSearch(t, e)

	if st, body := e.do("GET", "/search?q=%20%20android%20"); st != 200 || body != `{"results":[{"session_id":"s1","message_id":7}],"more":true}` {
		t.Fatalf("search: %d %s", st, body)
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	wantCode := "import sys; sys.path.insert(0, \"/fake/hermes-agent\"); import hermes_bootstrap\n" + searchScript
	if len(argv) != 5 || argv[0] != "-I" || argv[1] != "-c" || argv[2] != wantCode || argv[3] != "android" || argv[4] != "50" {
		t.Fatalf("argv %q", argv)
	}
	if home, _ := os.ReadFile(argvFile + ".home"); string(home) != "/fake/hermes-home" {
		t.Fatalf("HERMES_HOME %q", home)
	}
	if st, _ := e.do("GET", "/search?limit=100&q=android"); st != 200 {
		t.Fatalf("limit 100: %d", st)
	}
	if raw, _ := os.ReadFile(argvFile); !strings.HasSuffix(string(raw), "\x00android\x00100\x00") {
		t.Fatalf("argv with a limit %q", raw)
	}

	for name, c := range map[string]struct{ q, msg string }{
		"script failure": {"penpot", `"sqlite3.OperationalError: fts5: syntax error near penpot"`},
		"timeout":        {"slow", `"timed out"`},
		"not json":       {"garbage", `"the search script printed no result"`},
		"no results":     {"nothing", `"the search script printed no result"`},
	} {
		start := time.Now()
		if st, body := e.do("GET", "/search?q="+c.q); st != 502 || !strings.Contains(body, `"search_failed"`) || !strings.Contains(body, c.msg) {
			t.Errorf("%s: %d %s", name, st, body)
		}
		if time.Since(start) > 4*time.Second {
			t.Errorf("%s took %v", name, time.Since(start))
		}
	}
	if l := logs.String(); !strings.Contains(l, "search failed") || strings.Contains(l, "penpot") || strings.Contains(l, "android") {
		t.Fatalf("logs: %s", l)
	}

	if err := os.Remove(argvFile); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"", "q=", "q=a", "q=%20a%20%20", "q=" + strings.Repeat("x", 201), "q=ab&limit=0",
		"q=ab&limit=101", "q=ab&limit=-1", "q=ab&limit=x", "q=ab&limit=1.5"} {
		if st, body := e.do("GET", "/search?"+q); st != 400 || !strings.Contains(body, `"bad_request"`) {
			t.Errorf("%q: %d %s", q, st, body)
		}
	}
	if _, err := os.Stat(argvFile); err == nil {
		t.Fatal("the interpreter ran for a refused request")
	}
	if st, _ := e.do("GET", "/search?q="+url.QueryEscape(strings.Repeat("é", 200))); st == 400 { // characters, not bytes
		t.Fatal("200 characters refused")
	}
}

func TestSearchConcurrency(t *testing.T) {
	e := newEnv(t, "")
	_, dir := fakeSearch(t, e)
	starts := func() int {
		b, _ := os.ReadFile(filepath.Join(dir, "starts"))
		return len(b) / 2
	}
	waitFor := func(n int) {
		for deadline := time.Now().Add(3 * time.Second); starts() != n; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%d searches started, want %d", starts(), n)
			}
		}
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 3)
	search := func() {
		defer wg.Done()
		resp, err := http.Get(e.srv.URL + "/search?q=hold")
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
		statuses <- resp.StatusCode
	}
	wg.Add(2)
	go search()
	go search()
	waitFor(2)
	wg.Add(1)
	go search() // a third waits for a slot
	time.Sleep(150 * time.Millisecond)

	// A fourth gives up while waiting: it never runs.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/search?q=hold", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("a waiting search answered %d", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if starts() != 2 {
		t.Fatalf("%d searches running, want 2", starts())
	}

	e.write(filepath.Join(dir, "release"), "")
	wg.Wait()
	close(statuses)
	for st := range statuses {
		if st != 200 {
			t.Errorf("status %d", st)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if starts() != 3 {
		t.Fatalf("%d searches ran, want 3", starts())
	}
}
