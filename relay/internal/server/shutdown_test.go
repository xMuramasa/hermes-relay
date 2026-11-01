package server_test

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"
	"time"
)

// Shutdown during CreateRun waits for the prompt, so its run is stored and the next start tracks it to the end (#12).
func TestShutdownDuringCreateRunKeepsRun(t *testing.T) {
	fake := newFake(t, "plain")
	target, _ := url.Parse(fake.URL)
	entered, release := make(chan struct{}), make(chan struct{})
	var hold sync.Once
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ErrorLog = log.New(io.Discard, "", 0) // event streams cut by the shutdown
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/runs" {
			hold.Do(func() { close(entered); <-release })
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	fake.URL = proxy.URL

	r := newRelay(t, fake)
	cl := r.connect(r.pair("phone"))
	sid := fake.AddSession("api_server")
	const clientMsgID = "6f1c2a9e-1b7d-4c1e-9a35-000000000012"
	cl.send(map[string]any{"type": "prompt", "client_msg_id": clientMsgID, "session_id": sid,
		"text": secret("secret prompt text during shutdown", "prompt text")})
	<-entered

	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }() // SIGTERM while Hermes is creating the run
	select {
	case <-closed: // the store is already closed: the run can't be stored any more
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	<-closed

	r2 := r.restart()
	run, err := r2.st.RunByClientMsgID(context.Background(), clientMsgID)
	if err != nil {
		t.Fatalf("the run started during shutdown was not stored: %v", err)
	}
	for deadline := time.Now().Add(wait); run.Status != "completed"; {
		if time.Now().After(deadline) {
			t.Fatalf("run status %q after the restart, want completed", run.Status)
		}
		time.Sleep(20 * time.Millisecond)
		if run, err = r2.st.RunByID(context.Background(), run.RunID); err != nil {
			t.Fatal(err)
		}
	}
}
