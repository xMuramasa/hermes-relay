package hermes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
)

func newTest(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	c, err := New(srv.URL, "fake-key")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEventsParsing(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-key" || r.URL.Path != "/v1/runs/run_1/events" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, ": open\n\n")
		fmt.Fprint(w, "data: {\"event\":\"tool.started\",\"run_id\":\"run_1\",\"seq\":0,\"timestamp\":1.5}\r\n\r\n")
		fmt.Fprint(w, "data: not json\n\n")
		fmt.Fprint(w, ": keepalive\n\n")
		fmt.Fprint(w, "event: ignored\ndata: {\"event\":\"multi\",\ndata: \"seq\":1}\n\n")
		fmt.Fprint(w, "data: {\"event\":\"noseq\"}\n\n")
		fmt.Fprint(w, "data: {\"event\":\"run.completed\",\"seq\":2}\n\n")
		fmt.Fprint(w, "data: {\"event\":\"after\",\"seq\":3}\n\n")
	})
	var got []Frame
	stopped, err := c.Events(context.Background(), "run_1", func(f Frame) bool {
		got = append(got, f)
		return f.Event == "run.completed"
	})
	if !stopped || err != nil {
		t.Fatalf("stopped=%v err=%v", stopped, err)
	}
	if len(got) != 4 || got[0].Seq != 0 || got[0].RunID != "run_1" || got[0].Timestamp != 1.5 ||
		got[1].Event != "multi" || got[1].Seq != 1 || got[2].Seq != -1 || got[3].Seq != 2 {
		t.Fatalf("frames %+v", got)
	}
}

func TestEventsEndWithoutTerminal(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, ": open\n\ndata: {\"event\":\"x\",\"seq\":0}\n\n")
	})
	stopped, err := c.Events(context.Background(), "run_1", func(Frame) bool { return false })
	if stopped || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stopped=%v err=%v", stopped, err)
	}
}

func TestEventsDeadStream(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, ": open\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	c.DeadStream = 100 * time.Millisecond
	start := time.Now()
	_, err := c.Events(context.Background(), "run_1", func(Frame) bool { return false })
	if !errors.Is(err, ErrStreamDead) || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestEventsKeepalivesKeepStreamAlive(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 6; i++ {
			fmt.Fprint(w, ": keepalive\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"event\":\"run.completed\",\"seq\":0}\n\n")
	})
	c.DeadStream = 100 * time.Millisecond
	if stopped, err := c.Events(context.Background(), "run_1", func(Frame) bool { return true }); !stopped || err != nil {
		t.Fatalf("stopped=%v err=%v", stopped, err)
	}
}

func TestErrorMapping(t *testing.T) {
	status := 503
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":"x"}`)
	})
	cases := map[int]string{
		503: protocol.CodeHermesUnavailable, 404: protocol.CodeNotFound, 409: protocol.CodeBusy,
		429: protocol.CodeRateLimited, 400: protocol.CodeBadRequest, 401: protocol.CodeHermesError, 500: protocol.CodeHermesError,
	}
	for st, code := range cases {
		status = st
		_, err := c.CreateRun(context.Background(), "s", "hi", "k")
		pe := ProtocolError(err)
		if pe.Code != code {
			t.Errorf("%d → %s, want %s", st, pe.Code, code)
		}
		if st == 503 && (RetryAfterOf(err) != 12*time.Second || pe.Message != "Hermes is unavailable; retry after 12s") {
			t.Errorf("retry after: %v %q", RetryAfterOf(err), pe.Message)
		}
	}
	down, _ := New("http://127.0.0.1:1", "k")
	if _, err := down.GetRun(context.Background(), "run_1"); ProtocolError(err).Code != protocol.CodeHermesUnavailable {
		t.Fatalf("connection refused: %v", err)
	}
}

func TestRESTTimeout(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	c.RESTTimeout = 100 * time.Millisecond
	start := time.Now()
	_, err := c.GetSession(context.Background(), "s")
	if ProtocolError(err).Code != protocol.CodeHermesUnavailable || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestSessionShapes(t *testing.T) {
	if s, ok := SessionFrom([]byte(`{"session":{"id":"api_1","source":"api_server"}}`)); !ok || s.Source != "api_server" {
		t.Fatal(s)
	}
	if s, ok := SessionFrom([]byte(`{"id":"api_2","source":"telegram"}`)); !ok || s.ID != "api_2" {
		t.Fatal(s)
	}
	if _, ok := SessionFrom([]byte(`[]`)); ok {
		t.Fatal("array is not a session")
	}
	if l := SessionsFrom([]byte(`{"data":[{"id":"a","source":"cli"}],"has_more":false}`)); len(l) != 1 || l[0].Source != "cli" {
		t.Fatal(l)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if parseRetryAfter("30") != 30*time.Second || parseRetryAfter("") != 0 || parseRetryAfter("junk") != 0 ||
		parseRetryAfter("999999") != time.Hour {
		t.Fatal("retry-after parsing")
	}
	if d := parseRetryAfter(time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)); d < 50*time.Second || d > time.Minute {
		t.Fatalf("date: %v", d)
	}
}
