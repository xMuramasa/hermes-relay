package hub

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"github.com/xMuramasa/trismegistus/relay/internal/agents"
	"github.com/xMuramasa/trismegistus/relay/internal/fakehermes"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
)

// rewindCall is one request the fake helper got, with how many runs Hermes had created by then.
type rewindCall struct {
	session string
	before  int64
	creates int
}

// fakeHelper is the agents helper's rewind route on a temp Unix socket, answering status.
type fakeHelper struct {
	sock   string
	mu     sync.Mutex
	status int
	calls  []rewindCall
}

func newFakeHelper(t *testing.T, fake *fakehermes.Fake) *fakeHelper {
	t.Helper()
	f := &fakeHelper{sock: filepath.Join(t.TempDir(), "agents.sock"), status: http.StatusOK}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{id}/rewind", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Before int64 `json:"before_message_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		creates, _, _, _, _ := fake.Snapshot()
		f.mu.Lock()
		f.calls = append(f.calls, rewindCall{r.PathValue("id"), body.Before, creates})
		st := f.status
		f.mu.Unlock()
		w.WriteHeader(st)
		switch st {
		case http.StatusOK:
			_, _ = w.Write([]byte(`{"rewound":10,"turns":2}`))
		case http.StatusNotFound:
			_, _ = w.Write([]byte(`{"error":{"code":"not_a_user_message","message":"not one of the session's user messages"}}`))
		default:
			_, _ = w.Write([]byte(`{"error":{"code":"rewind_failed","message":"timed out"}}`))
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeHelper) set(status int) { f.mu.Lock(); f.status = status; f.mu.Unlock() }

func (f *fakeHelper) snapshot() []rewindCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rewindCall(nil), f.calls...)
}

// A prompt with rewind_to rewinds through the helper before Hermes starts the run, never again for a replay, never
// when the session is busy, and maps the helper's failures to the protocol's codes.
func TestPromptRewind(t *testing.T) {
	h, _, fake, rec, sid := newTestHub(t)
	fh := newFakeHelper(t, fake)
	h.SetAgents(agents.New(fh.sock))
	prompt := func(clientMsgID string, rewindTo int64) (runID string, replayed bool, err error) {
		err = h.Prompt(context.Background(), 1, "mac", clientMsgID, sid, "what is 3+3?", nil, rewindTo,
			func(id string, r bool) { runID, replayed = id, r })
		return
	}
	creates := func() int { n, _, _, _, _ := fake.Snapshot(); return n }

	run, replayed, err := prompt("6f1c2a9e-1b7d-4c1e-9a35-000000000101", 4123)
	if err != nil || replayed || run == "" {
		t.Fatalf("prompt: %q %v %v", run, replayed, err)
	}
	rec.until(t, isRunEnd)
	if calls := fh.snapshot(); len(calls) != 1 || calls[0] != (rewindCall{sid, 4123, 0}) {
		t.Fatalf("helper calls %+v, want one for %s before 4123 ahead of CreateRun", calls, sid)
	}
	if creates() != 1 {
		t.Fatalf("%d CreateRun calls, want 1", creates())
	}

	// A resend of the same client_msg_id is answered from the replay check: nothing is rewound again.
	if again, replayed, err := prompt("6f1c2a9e-1b7d-4c1e-9a35-000000000101", 4123); err != nil || !replayed || again != run {
		t.Fatalf("resend: %q %v %v", again, replayed, err)
	}
	if n := len(fh.snapshot()); n != 1 {
		t.Fatalf("resend rewound again: %d calls", n)
	}

	// Busy is refused before the helper is asked.
	s := h.lockSession(sid)
	s.starting = true
	s.mu.Unlock()
	_, _, err = prompt("6f1c2a9e-1b7d-4c1e-9a35-000000000102", 4123)
	s = h.lockSession(sid)
	s.starting = false
	h.unlockSession(s)
	if protocol.CodeOf(err) != protocol.CodeBusy || len(fh.snapshot()) != 1 {
		t.Fatalf("busy: %v, %d helper calls", err, len(fh.snapshot()))
	}

	// Helper failures: no run is started, and the session is free for the next prompt.
	for i, c := range []struct {
		status int
		code   string
	}{{http.StatusNotFound, protocol.CodeNotFound}, {http.StatusBadGateway, protocol.CodeHermesError}, {http.StatusInternalServerError, protocol.CodeHermesError}} {
		fh.set(c.status)
		id := "6f1c2a9e-1b7d-4c1e-9a35-00000000011" + string(rune('0'+i))
		if _, _, err := prompt(id, 7); protocol.CodeOf(err) != c.code {
			t.Errorf("helper %d: %v, want %s", c.status, err, c.code)
		}
	}
	if n := len(fh.snapshot()); n != 4 || creates() != 1 {
		t.Fatalf("%d helper calls and %d CreateRun calls, want 4 and 1", n, creates())
	}
	h.SetAgents(agents.New(filepath.Join(t.TempDir(), "missing.sock")))
	if _, _, err := prompt("6f1c2a9e-1b7d-4c1e-9a35-000000000120", 7); protocol.CodeOf(err) != protocol.CodeHermesUnavailable {
		t.Errorf("helper not running: %v", err)
	}
	h.SetAgents(nil)
	if _, _, err := prompt("6f1c2a9e-1b7d-4c1e-9a35-000000000121", 7); protocol.CodeOf(err) != protocol.CodeHermesUnavailable {
		t.Errorf("no helper configured: %v", err)
	}

	// A rewind that worked stays when Hermes then refuses the run; the error goes back as for any prompt.
	h.SetAgents(agents.New(fh.sock))
	fh.set(http.StatusOK)
	fake.FailCreateRun(http.StatusBadRequest, "")
	_, _, err = prompt("6f1c2a9e-1b7d-4c1e-9a35-000000000130", 7)
	fake.FailCreateRun(0, "")
	if err == nil || len(fh.snapshot()) != 5 {
		t.Fatalf("rewind then failed run: %v, %d helper calls", err, len(fh.snapshot()))
	}

	// Without rewind_to the helper is never asked.
	if _, _, err := prompt("6f1c2a9e-1b7d-4c1e-9a35-000000000140", 0); err != nil || len(fh.snapshot()) != 5 {
		t.Fatalf("plain prompt: %v, %d helper calls", err, len(fh.snapshot()))
	}
	rec.until(t, isRunEnd)
}
