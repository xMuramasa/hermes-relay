package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/xMuramasa/trismegistus/relay/internal/fakehermes"
	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/hub"
	"github.com/xMuramasa/trismegistus/relay/internal/pairing"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/server"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

const fakeKey = "fake-hermes-key-0123456789abcdef"

// Every log line of the suite goes here; TestMain checks it for secrets at the end.
var (
	suiteLogs   syncBuffer
	secretsMu   sync.Mutex
	secrets     = map[string]string{fakeKey: "Hermes key"}
	validatorMu sync.Mutex
	validator   = protocol.MustValidator()
)

// secret registers a string that must never appear in the logs.
func secret(s, what string) string {
	secretsMu.Lock()
	secrets[s] = what
	secretsMu.Unlock()
	return s
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestMain(m *testing.M) {
	code := m.Run()
	logs := suiteLogs.String()
	secretsMu.Lock()
	for s, what := range secrets {
		if strings.Contains(logs, s) {
			fmt.Fprintf(os.Stderr, "FAIL: the relay logged a %s\n", what)
			code = 1
		}
	}
	n := len(secrets)
	secretsMu.Unlock()
	if strings.Count(logs, "\n") < 20 {
		fmt.Fprintln(os.Stderr, "FAIL: suspiciously few log lines captured")
		code = 1
	}
	if code == 0 {
		fmt.Printf("log check: %d log lines, %d secrets, none logged\n", strings.Count(logs, "\n"), n)
	}
	os.Exit(code)
}

func fixture(t *testing.T, name string) *fakehermes.Fixture {
	t.Helper()
	fx, err := fakehermes.LoadFixture(filepath.Join("..", "..", "..", "probe", "fixtures", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

// relay is one relay process (store + hub + server) over a data dir.
type relay struct {
	t     *testing.T
	dir   string
	fake  *fakehermes.Fake
	st    *store.Store
	hub   *hub.Hub
	srv   *server.Server
	http  *httptest.Server
	stop  context.CancelFunc
	done  chan struct{}
	opts  server.Options
	once  sync.Once
	hopts hub.Options
}

func fastServerOptions() server.Options {
	o := server.DefaultOptions()
	o.Version = "test"
	o.AuthTimeout = 300 * time.Millisecond
	o.RevokeCheck = 50 * time.Millisecond
	return o
}

func fastHubOptions() hub.Options {
	return hub.Options{PollMin: 10 * time.Millisecond, PollMax: 50 * time.Millisecond, GiveUp: 3 * time.Second, MaxReplay: 5000}
}

func newFake(t *testing.T, def string) *fakehermes.Fake {
	f := fakehermes.New(fakeKey, fixture(t, def))
	t.Cleanup(f.Close)
	return f
}

func startRelay(t *testing.T, fake *fakehermes.Fake, dir string, opts server.Options, hopts hub.Options) *relay {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	hc, err := hermes.New(fake.URL, fakeKey)
	if err != nil {
		t.Fatal(err)
	}
	hc.DeadStream = 400 * time.Millisecond
	log := slog.New(slog.NewJSONHandler(&suiteLogs, &slog.HandlerOptions{Level: slog.LevelDebug})).With("test", t.Name())
	h := hub.New(st, hc, log, hopts)
	srv, err := server.New(opts, st, h, hc, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r := &relay{t: t, dir: dir, fake: fake, st: st, hub: h, srv: srv, stop: cancel, done: make(chan struct{}), opts: opts, hopts: hopts}
	go func() { srv.Run(ctx); close(r.done) }()
	r.http = httptest.NewServer(srv.Handler())
	t.Cleanup(r.Close)
	return r
}

func newRelay(t *testing.T, fake *fakehermes.Fake) *relay {
	return startRelay(t, fake, t.TempDir(), fastServerOptions(), fastHubOptions())
}

// Close shuts the relay down like SIGTERM does.
func (r *relay) Close() {
	r.once.Do(func() {
		r.srv.CloseSockets()
		r.http.CloseClientConnections()
		r.http.Close()
		r.hub.Close()
		r.stop()
		<-r.done
		r.st.Close()
	})
}

// restart closes this relay and starts another on the same database.
func (r *relay) restart() *relay {
	r.Close()
	return startRelay(r.t, r.fake, r.dir, r.opts, r.hopts)
}

// pair makes a pairing code (as the CLI does) and redeems it over HTTP. It returns the token.
func (r *relay) pair(name string) string {
	r.t.Helper()
	code, hash := pairing.NewCode()
	secret(code, "pairing code")
	if _, err := r.st.CreatePairCode(context.Background(), name, hash, pairing.CodeTTL, false, 5); err != nil {
		r.t.Fatal(err)
	}
	status, body := r.do("POST", "/api/pair", "", map[string]any{"code": code})
	if status != 200 {
		r.t.Fatalf("pair: %d %s", status, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		r.t.Fatalf("pair: %s", body)
	}
	return secret(out.Token, "device token")
}

// do sends a REST request with an optional bearer token and JSON body.
func (r *relay) do(method, path, token string, body any) (int, []byte) {
	r.t.Helper()
	return r.doHeaders(method, path, token, body, nil)
}

func (r *relay) doHeaders(method, path, token string, body any, hdr map[string]string) (int, []byte) {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		default:
			j, _ := json.Marshal(b)
			rd = bytes.NewReader(j)
		}
	}
	req, _ := http.NewRequest(method, r.http.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// client is a test WebSocket client; every message it receives must match the schema.
type client struct {
	t    *testing.T
	c    *websocket.Conn
	msgs chan msg
	done chan struct{}
	err  error // read error, set before done closes
}

type msg map[string]any

func (m msg) str(k string) string { s, _ := m[k].(string); return s }
func (m msg) num(k string) int64  { f, _ := m[k].(float64); return int64(f) }
func (m msg) data() msg           { d, _ := m["data"].(map[string]any); return d }

func (r *relay) dialRaw() *websocket.Conn {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.http.URL, "http")+"/ws", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	c.SetReadLimit(1 << 20)
	return c
}

// connect opens a socket, authenticates and waits for ready.
func (r *relay) connect(token string) *client {
	r.t.Helper()
	cl := &client{t: r.t, c: r.dialRaw(), msgs: make(chan msg, 10000), done: make(chan struct{})}
	go cl.read()
	r.t.Cleanup(func() { _ = cl.c.CloseNow() })
	cl.send(map[string]any{"type": "auth", "token": token})
	if m := cl.next(); m.str("type") != "ready" {
		r.t.Fatalf("expected ready, got %v", m)
	}
	return cl
}

func (cl *client) read() {
	defer close(cl.done)
	for {
		_, b, err := cl.c.Read(context.Background())
		if err != nil {
			cl.err = err
			return
		}
		validatorMu.Lock()
		verr := validator.ValidateDetail(b)
		validatorMu.Unlock()
		if verr != nil {
			cl.t.Errorf("relay sent a message that fails the schema: %s: %v", b, verr)
		}
		var m msg
		if err := json.Unmarshal(b, &m); err != nil {
			cl.t.Errorf("relay sent invalid JSON: %s", b)
			continue
		}
		cl.msgs <- m
	}
}

func (cl *client) send(v any) {
	cl.t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cl.c.Write(ctx, websocket.MessageText, b); err != nil {
		cl.t.Fatalf("send: %v", err)
	}
}

const wait = 5 * time.Second

func (cl *client) next() msg {
	cl.t.Helper()
	select {
	case m := <-cl.msgs:
		return m
	case <-cl.done:
		cl.t.Fatalf("socket closed: %v", cl.err)
	case <-time.After(wait):
		cl.t.Fatal("timed out waiting for a message")
	}
	return nil
}

// until collects messages until one satisfies pred (included).
func (cl *client) until(pred func(msg) bool) []msg {
	cl.t.Helper()
	var got []msg
	for {
		m := cl.next()
		got = append(got, m)
		if pred(m) {
			return got
		}
	}
}

// quiet asserts nothing arrives for d.
func (cl *client) quiet(d time.Duration) {
	cl.t.Helper()
	select {
	case m := <-cl.msgs:
		cl.t.Fatalf("unexpected message %v", m)
	case <-time.After(d):
	}
}

// closed waits for the relay to close the socket and returns the close status.
func (cl *client) closed() websocket.StatusCode {
	cl.t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case <-cl.msgs:
		case <-cl.done:
			return websocket.CloseStatus(cl.err)
		case <-deadline:
			cl.t.Fatal("socket was not closed")
			return 0
		}
	}
}

func isType(typ string) func(msg) bool { return func(m msg) bool { return m.str("type") == typ } }

func isTerminal(m msg) bool {
	if m.str("type") != "event" {
		return false
	}
	switch m.str("kind") {
	case "run.completed", "run.failed", "run.cancelled":
		return true
	}
	return false
}

// isRunEnd matches the run status message that follows a run's last event.
func isRunEnd(m msg) bool {
	if m.str("type") != "run" {
		return false
	}
	switch m.str("status") {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

// events keeps only event messages.
func events(ms []msg) []msg {
	var out []msg
	for _, m := range ms {
		if m.str("type") == "event" {
			out = append(out, m)
		}
	}
	return out
}

func kinds(evs []msg) string {
	var k []string
	for _, e := range evs {
		k = append(k, e.str("kind"))
	}
	return strings.Join(k, " ")
}

// checkSeqs asserts events are numbered from..from+n-1 without gaps.
func checkSeqs(t *testing.T, evs []msg, from int64) {
	t.Helper()
	for i, e := range evs {
		if e.num("seq") != from+int64(i) {
			t.Fatalf("event %d has seq %d, want %d (%s)", i, e.num("seq"), from+int64(i), kinds(evs))
		}
	}
}

var promptN int

// prompt sends a prompt with a unique text and client_msg_id and returns the ack.
func (cl *client) prompt(sessionID string) (clientMsgID string, ack msg) {
	cl.t.Helper()
	promptN++
	clientMsgID = fmt.Sprintf("6f1c2a9e-1b7d-4c1e-9a35-%012d", promptN)
	text := secret(fmt.Sprintf("secret prompt text %d about the ocean", promptN), "prompt text")
	cl.send(map[string]any{"type": "prompt", "client_msg_id": clientMsgID, "session_id": sessionID, "text": text})
	got := cl.until(func(m msg) bool { return m.str("type") == "ack" || m.str("type") == "error" })
	return clientMsgID, got[len(got)-1]
}

func (cl *client) subscribe(sessionID string) {
	cl.send(map[string]any{"type": "subscribe", "session_id": sessionID})
	cl.send(map[string]any{"type": "ping"})
	cl.until(isType("pong"))
}
