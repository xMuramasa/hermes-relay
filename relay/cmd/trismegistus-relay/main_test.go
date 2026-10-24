package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func runCLI(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, func(k string) string { return env[k] }, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestHelpAndVersion(t *testing.T) {
	for _, a := range []string{"--help", "-h", "help"} {
		if code, out, _ := runCLI(t, nil, a); code != 0 || !strings.Contains(out, "pair --name NAME") {
			t.Fatalf("%s: %d %q", a, code, out)
		}
	}
	if code, out, _ := runCLI(t, nil, "version"); code != 0 || strings.TrimSpace(out) != version {
		t.Fatal(out)
	}
	if code, _, _ := runCLI(t, nil, "bogus"); code != 2 {
		t.Fatal("unknown command")
	}
}

func TestServeNeedsKey(t *testing.T) {
	if code, _, errOut := runCLI(t, map[string]string{"DATA_DIR": t.TempDir()}, "serve"); code != 2 || !strings.Contains(errOut, "HERMES_API_KEY") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestPairAndDevices(t *testing.T) {
	env := map[string]string{"DATA_DIR": t.TempDir(), "PUBLIC_URL": "https://relay.example"}
	if code, _, errOut := runCLI(t, map[string]string{"DATA_DIR": env["DATA_DIR"]}, "pair", "--name", "pixel-8"); code != 2 || !strings.Contains(errOut, "PUBLIC_URL") {
		t.Fatalf("pair without PUBLIC_URL: %d %q", code, errOut)
	}
	if code, _, _ := runCLI(t, env, "pair"); code != 2 {
		t.Fatal("pair without --name")
	}
	if code, _, _ := runCLI(t, env, "pair", "--name", "bad name"); code != 2 {
		t.Fatal("bad name accepted")
	}
	code, out, errOut := runCLI(t, env, "pair", "--name", "pixel-8")
	if code != 0 || !strings.Contains(out, "Code:    ") || !strings.Contains(out, "https://relay.example") || !strings.Contains(out, "▀") {
		t.Fatalf("pair: %d %q %q", code, out, errOut)
	}
	if code, out, _ := runCLI(t, env, "devices", "list"); code != 0 || !strings.Contains(out, "NAME") {
		t.Fatalf("list: %d %q", code, out)
	}
	if code, _, errOut := runCLI(t, env, "devices", "revoke", "pixel-8"); code != 1 || !strings.Contains(errOut, "no device") {
		t.Fatalf("revoke unpaired: %d %q", code, errOut)
	}
	if code, _, _ := runCLI(t, env, "devices", "frobnicate"); code != 2 {
		t.Fatal("bad devices subcommand")
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		w.WriteHeader(404)
	}))
	defer ok.Close()
	addr := strings.TrimPrefix(ok.URL, "http://")
	if code, _, _ := runCLI(t, map[string]string{"RELAY_ADDR": addr}, "healthcheck"); code != 0 {
		t.Fatal("healthy relay reported unhealthy")
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	if code, _, _ := runCLI(t, map[string]string{"RELAY_ADDR": dead}, "healthcheck"); code != 1 {
		t.Fatal("dead relay reported healthy")
	}
}

func TestServeStartsAndStops(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	env := map[string]string{"DATA_DIR": t.TempDir(), "HERMES_API_KEY": "fake-key", "RELAY_ADDR": addr, "HERMES_URL": "http://127.0.0.1:1"}
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuf
	done := make(chan int)
	go func() { done <- run(ctx, nil, func(k string) string { return env[k] }, &out, &out) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if code, _, _ := runCLI(t, map[string]string{"RELAY_ADDR": addr}, "healthcheck"); code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay did not become healthy: %s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel() // what SIGTERM does through signal.NotifyContext
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("relay did not stop")
	}
	logs := out.String()
	if !strings.Contains(logs, `"msg":"relay started"`) || !strings.Contains(logs, `"msg":"stopped"`) || strings.Contains(logs, "fake-key") {
		t.Fatalf("logs: %s", logs)
	}
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
