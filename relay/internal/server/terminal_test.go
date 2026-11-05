package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// fakeTerm is the host helper: it echoes data frames, answers "exit\n" with an exit frame and "flood\n" with output
// that never stops.
type fakeTerm struct {
	t       *testing.T
	path    string
	answer  string
	opens   chan map[string]any
	resizes chan string
	closed  chan struct{}
	flooded atomic.Int64
}

func newFakeTerm(t *testing.T, answer string) *fakeTerm {
	t.Helper()
	dir, err := os.MkdirTemp("", "term") // short: socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeTerm{t: t, path: filepath.Join(dir, "term.sock"), answer: answer,
		opens: make(chan map[string]any, 8), resizes: make(chan string, 8), closed: make(chan struct{}, 8)}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func termFrame(typ byte, p []byte) []byte {
	return append(binary.BigEndian.AppendUint16([]byte{typ}, uint16(len(p))), p...)
}

func (f *fakeTerm) serve(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	var open map[string]any
	_ = json.Unmarshal(line, &open)
	f.opens <- open
	if _, err := conn.Write([]byte(f.answer + "\n")); err != nil {
		return
	}
	if strings.Contains(f.answer, "error") {
		return
	}
	hdr := make([]byte, 3)
	for {
		if _, err := readFull(br, hdr); err != nil {
			f.closed <- struct{}{}
			return
		}
		p := make([]byte, binary.BigEndian.Uint16(hdr[1:]))
		if _, err := readFull(br, p); err != nil {
			f.closed <- struct{}{}
			return
		}
		switch {
		case hdr[0] == 1:
			f.resizes <- fmt.Sprintf("%d %d", binary.BigEndian.Uint16(p), binary.BigEndian.Uint16(p[2:]))
		case string(p) == "exit\n":
			_, _ = conn.Write(termFrame(2, binary.BigEndian.AppendUint32(nil, 7)))
			return
		case string(p) == "die\n":
			return
		case string(p) == "flood\n":
			go f.flood(conn)
		default:
			_, _ = conn.Write(termFrame(3, nil)) // pings are ignored
			if _, err := conn.Write(termFrame(0, p)); err != nil {
				return
			}
		}
	}
}

func readFull(br *bufio.Reader, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := br.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (f *fakeTerm) flood(conn net.Conn) {
	frame := termFrame(0, bytes.Repeat([]byte("y"), 32<<10))
	for {
		if _, err := conn.Write(frame); err != nil {
			return
		}
		f.flooded.Add(1 << 15)
	}
}

// waitClosed waits for the helper to see its connection close (what ends the shell).
func (f *fakeTerm) waitClosed() {
	f.t.Helper()
	select {
	case <-f.closed:
	case <-time.After(wait):
		f.t.Fatal("the helper's connection stayed open")
	}
}

var termSchema = func() *jsonschema.Schema {
	file, err := os.Open(filepath.Join("..", "..", "..", "protocol", "terminal.schema.json"))
	if err != nil {
		panic(err)
	}
	defer file.Close()
	doc, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		panic(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("terminal.schema.json", doc); err != nil {
		panic(err)
	}
	return c.MustCompile("terminal.schema.json")
}()

// termClient is the Mac app on /api/terminal; every text frame must match protocol/terminal.schema.json.
type termClient struct {
	t *testing.T
	c *websocket.Conn
}

func (r *relay) termDial() *termClient {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.http.URL, "http")+"/api/terminal", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	c.SetReadLimit(1 << 20)
	r.t.Cleanup(func() { _ = c.CloseNow() })
	return &termClient{t: r.t, c: c}
}

func (tc *termClient) text(v any) {
	tc.t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if err := tc.c.Write(ctx, websocket.MessageText, b); err != nil {
		tc.t.Fatal(err)
	}
}

func (tc *termClient) binary(p []byte) {
	tc.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if err := tc.c.Write(ctx, websocket.MessageBinary, p); err != nil {
		tc.t.Fatal(err)
	}
}

// next reads a frame; a text frame comes back as msg, a binary one as bytes.
func (tc *termClient) next() (msg, []byte) {
	tc.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	typ, b, err := tc.c.Read(ctx)
	if err != nil {
		tc.t.Fatalf("read: %v", err)
	}
	if typ == websocket.MessageBinary {
		return nil, b
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		tc.t.Fatal(err)
	}
	if err := termSchema.Validate(inst); err != nil {
		tc.t.Fatalf("%s: %v", b, err)
	}
	var m msg
	_ = json.Unmarshal(b, &m)
	return m, nil
}

// closed waits for the relay to close the socket and returns the close code.
func (tc *termClient) closed() websocket.StatusCode {
	tc.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	for {
		if _, _, err := tc.c.Read(ctx); err != nil {
			if ctx.Err() != nil {
				tc.t.Fatal("the socket stayed open")
			}
			return websocket.CloseStatus(err)
		}
	}
}

// open does the handshake and returns the relay's answer.
func (r *relay) termOpen(token string) (*termClient, msg) {
	r.t.Helper()
	tc := r.termDial()
	tc.text(map[string]any{"type": "auth", "token": token})
	tc.text(map[string]any{"type": "open", "cols": 80, "rows": 24})
	m, _ := tc.next()
	return tc, m
}

func (tc *termClient) wantError(m msg, code, inMessage string) {
	tc.t.Helper()
	if m.str("type") != "error" || m.str("code") != code || !strings.Contains(m.str("message"), inMessage) {
		tc.t.Fatalf("want error %s with %q, got %v", code, inMessage, m)
	}
	if got := tc.closed(); got != websocket.StatusPolicyViolation {
		tc.t.Fatalf("close code %d", got)
	}
}

func (r *relay) grant(name string, on bool) {
	r.t.Helper()
	if err := r.st.SetDeviceTerminal(context.Background(), name, on); err != nil {
		r.t.Fatal(err)
	}
}

// granted is a relay with a fake helper and a manager holding the grant.
func grantedRelay(t *testing.T) (*relay, *fakeTerm, string) {
	t.Helper()
	r := newRelay(t, newFake(t, "plain"))
	f := newFakeTerm(t, `{"host":"optiplex","shell":"/bin/bash"}`)
	r.srv.SetTerminal(f.path)
	mac := r.pairAs("macbook", store.RoleManager)
	r.grant("macbook", true)
	return r, f, mac
}

func TestTerminalAuth(t *testing.T) {
	r, _, _ := grantedRelay(t)
	tc := r.termDial()
	tc.text(map[string]any{"type": "auth", "token": "trs_FAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAK"})
	m, _ := tc.next()
	tc.wantError(m, "unauthorized", "unauthorized")

	tc = r.termDial()
	tc.text(map[string]any{"type": "open", "cols": 80, "rows": 24})
	m, _ = tc.next()
	tc.wantError(m, "unauthorized", "auth")

	// No auth at all: closed after the timeout.
	if got := r.termDial().closed(); got != websocket.StatusPolicyViolation {
		t.Fatalf("auth timeout: %d", got)
	}
}

func TestTerminalBadOpen(t *testing.T) {
	r, _, mac := grantedRelay(t)
	for _, open := range []any{
		map[string]any{"type": "resize", "cols": 80, "rows": 24},
		map[string]any{"type": "open", "cols": 0, "rows": 24},
		map[string]any{"type": "open", "cols": 80, "rows": 1001},
		map[string]any{"type": "open"},
	} {
		tc := r.termDial()
		tc.text(map[string]any{"type": "auth", "token": mac})
		tc.text(open)
		m, _ := tc.next()
		tc.wantError(m, "bad_request", "open")
	}
	// Authenticated but silent: dropped, nothing opened.
	tc := r.termDial()
	tc.text(map[string]any{"type": "auth", "token": mac})
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if _, _, err := tc.c.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("open timeout: %v", err)
	}
}

func TestTerminalGrantAndStream(t *testing.T) {
	r, f, mac := grantedRelay(t)
	r.grant("macbook", false)
	tc, m := r.termOpen(mac)
	tc.wantError(m, "forbidden", "devices terminal macbook on")

	// It takes effect at once, and a device's grant is its own.
	other := r.pairAs("studio", store.RoleManager)
	r.grant("macbook", true)
	tc, m = r.termOpen(other)
	tc.wantError(m, "forbidden", "devices terminal studio on")
	tc, m = r.termOpen(mac)
	if m.str("type") != "opened" || m.str("host") != "optiplex" || m.str("shell") != "/bin/bash" {
		t.Fatalf("opened: %v", m)
	}
	if o := <-f.opens; o["device"] != "macbook" || o["cols"] != float64(80) || o["rows"] != float64(24) {
		t.Fatalf("open line: %v", o)
	}

	stream := secret("FAKEFAKE-terminal-stream-content", "terminal stream")
	tc.binary([]byte(stream))
	if _, b := tc.next(); string(b) != stream {
		t.Fatalf("echo: %q", b)
	}
	tc.text(map[string]any{"type": "resize", "cols": 132, "rows": 50})
	if got := <-f.resizes; got != "132 50" {
		t.Fatalf("resize: %s", got)
	}
	tc.text(map[string]any{"type": "resize", "cols": 0, "rows": 50})
	if m, _ := tc.next(); m.str("code") != "bad_request" {
		t.Fatalf("bad resize: %v", m)
	}
	tc.binary([]byte("exit\n"))
	if m, _ := tc.next(); m.str("type") != "exit" || m.num("status") != 7 {
		t.Fatalf("exit: %v", m)
	}
	if got := tc.closed(); got != websocket.StatusNormalClosure {
		t.Fatalf("close code %d", got)
	}
}

// A viewer can't hold the grant (the store refuses it); one whose grant was set behind the store's back is still
// refused here, by role.
func TestTerminalViewerForbidden(t *testing.T) {
	r, _, _ := grantedRelay(t)
	phone := r.pair("s21")
	if err := r.st.SetDeviceTerminal(context.Background(), "s21", true); !errors.Is(err, store.ErrNotManager) {
		t.Fatalf("grant for a viewer: %v", err)
	}
	tc, m := r.termOpen(phone)
	tc.wantError(m, "forbidden", "viewer")
}

func TestTerminalNotConfigured(t *testing.T) {
	r, _, mac := grantedRelay(t)
	r.srv.SetTerminal("")
	tc, m := r.termOpen(mac)
	tc.wantError(m, "not_configured", "TERM_SOCKET")
}

func TestTerminalUnavailable(t *testing.T) {
	r, _, mac := grantedRelay(t)
	r.srv.SetTerminal(filepath.Join(t.TempDir(), "missing.sock"))
	tc, m := r.termOpen(mac)
	tc.wantError(m, "unavailable", "helper")
}

func TestTerminalBusy(t *testing.T) {
	r, f, mac := grantedRelay(t)
	f.answer = `{"error":"busy"}`
	tc, m := r.termOpen(mac)
	tc.wantError(m, "busy", "4 terminals")
}

func TestTerminalShellFailsToStart(t *testing.T) {
	r, f, mac := grantedRelay(t)
	f.answer = `{"error":"start_failed"}`
	tc, m := r.termOpen(mac)
	tc.wantError(m, "unavailable", "helper")
}

func TestTerminalHelperDies(t *testing.T) {
	r, _, mac := grantedRelay(t)
	tc, _ := r.termOpen(mac)
	tc.binary([]byte("die\n"))
	if got := tc.closed(); got != websocket.StatusGoingAway {
		t.Fatalf("close code %d", got)
	}
}

// Each way of losing access closes an open terminal within the poll, the helper's connection with it.
func TestTerminalAccessEnds(t *testing.T) {
	for name, end := range map[string]func(*relay) error{
		"revoke":   func(r *relay) error { return r.st.RevokeDevice(context.Background(), "macbook") },
		"grantoff": func(r *relay) error { return r.st.SetDeviceTerminal(context.Background(), "macbook", false) },
		"demote":   func(r *relay) error { return r.st.SetDeviceRole(context.Background(), "macbook", store.RoleViewer) },
	} {
		t.Run(name, func(t *testing.T) {
			r, f, mac := grantedRelay(t)
			other := r.pairAs("studio", store.RoleManager)
			r.grant("studio", true)
			tc, m := r.termOpen(mac)
			kept, m2 := r.termOpen(other)
			if m.str("type") != "opened" || m2.str("type") != "opened" {
				t.Fatalf("%v %v", m, m2)
			}
			tc.binary([]byte("still on\n"))
			tc.next()
			start := time.Now()
			if err := end(r); err != nil {
				t.Fatal(err)
			}
			if got := tc.closed(); got != websocket.StatusPolicyViolation {
				t.Fatalf("close code %d", got)
			}
			f.waitClosed()
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("took %v", d)
			}
			kept.binary([]byte("mine\n")) // another device's terminal stays
			if _, b := kept.next(); string(b) != "mine\n" {
				t.Fatalf("other terminal: %q", b)
			}
		})
	}
}

func TestTerminalAppClosesEndsShell(t *testing.T) {
	r, f, mac := grantedRelay(t)
	tc, _ := r.termOpen(mac)
	if err := tc.c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	f.waitClosed()
}

func TestTerminalShutdown(t *testing.T) {
	r, f, mac := grantedRelay(t)
	tc, _ := r.termOpen(mac)
	go r.Close()
	if got := tc.closed(); got != websocket.StatusGoingAway {
		t.Fatalf("close code %d", got)
	}
	f.waitClosed()
}

func TestTerminalFrameLimit(t *testing.T) {
	r, f, mac := grantedRelay(t)
	tc, _ := r.termOpen(mac)
	full := bytes.Repeat([]byte("x"), 32<<10)
	tc.binary(full)
	if _, b := tc.next(); len(b) != len(full) {
		t.Fatalf("echoed %d bytes", len(b))
	}
	tc.binary(append(full, 'x'))
	if got := tc.closed(); got != websocket.StatusMessageTooBig {
		t.Fatalf("close code %d", got)
	}
	f.waitClosed()
}

// An app that stops reading stops the helper's writes, and the terminal stays up for when it reads again.
func TestTerminalBackpressure(t *testing.T) {
	r, f, mac := grantedRelay(t)
	tc, _ := r.termOpen(mac)
	tc.binary([]byte("flood\n"))
	var sent int64
	for end := time.Now().Add(wait); ; time.Sleep(300 * time.Millisecond) {
		n := f.flooded.Load()
		if n > 0 && n == sent {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the helper never stalled: %d bytes", n)
		}
		sent = n
	}
	if sent > 64<<20 {
		t.Fatalf("the relay took %d bytes for an app that isn't reading", sent)
	}
	var read int64
	for read <= sent {
		_, b := tc.next()
		read += int64(len(b))
	}
	for end := time.Now().Add(wait); f.flooded.Load() == sent; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the helper did not resume")
		}
	}
}
