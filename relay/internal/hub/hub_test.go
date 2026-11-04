package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/fakehermes"
	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

const testKey = "fake-hermes-key-0123456789abcdef"

// flakyStore is the real store with failures and delays injected on demand.
type flakyStore struct {
	*store.Store
	failAppend  func(evs []store.NewEvent) bool // AppendEvents fails when it returns true
	afterLookup func()                          // runs after RunByClientMsgID read the database, before it returns
}

func (f *flakyStore) RunByClientMsgID(ctx context.Context, clientMsgID string) (store.Run, error) {
	r, err := f.Store.RunByClientMsgID(ctx, clientMsgID)
	if f.afterLookup != nil {
		f.afterLookup()
	}
	return r, err
}

func (f *flakyStore) AppendEvents(ctx context.Context, sessionID, runID string, evs []store.NewEvent, upd store.RunUpdate) ([]store.Event, error) {
	if f.failAppend != nil && f.failAppend(evs) {
		return nil, errors.New("injected: database is locked")
	}
	return f.Store.AppendEvents(ctx, sessionID, runID, evs, upd)
}

// recorder is a subscriber that keeps every message.
type recorder struct{ msgs chan map[string]any }

func (r *recorder) Deliver(b []byte) {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	r.msgs <- m
}

// until collects messages until one satisfies pred (included).
func (r *recorder) until(t *testing.T, pred func(map[string]any) bool) []map[string]any {
	t.Helper()
	var got []map[string]any
	for {
		select {
		case m := <-r.msgs:
			got = append(got, m)
			if pred(m) {
				return got
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out; got %v", got)
		}
	}
}

func isRunEnd(m map[string]any) bool {
	return m["type"] == "run" && protocol.Terminal(m["status"].(string))
}

// newTestHub starts a hub over a temp database and a fake Hermes replaying the plain fixture. It returns the hub,
// its store and a subscriber of a fresh api_server session.
func newTestHub(t *testing.T) (*Hub, *flakyStore, *fakehermes.Fake, *recorder, string) {
	t.Helper()
	fx, err := fakehermes.LoadFixture(filepath.Join("..", "..", "..", "probe", "fixtures", "plain.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	fake := fakehermes.New(testKey, fx)
	t.Cleanup(fake.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	hc, err := hermes.New(fake.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	hc.DeadStream = 400 * time.Millisecond
	h := New(st, hc, slog.New(slog.DiscardHandler), Options{PollMin: 10 * time.Millisecond, PollMax: 50 * time.Millisecond, GiveUp: 3 * time.Second, MaxReplay: 5000})
	fs := &flakyStore{Store: st}
	h.store = fs
	t.Cleanup(func() { h.Close(); st.Close() })
	sid := fake.AddSession(SourceAPIServer)
	rec := &recorder{msgs: make(chan map[string]any, 1000)}
	if err := h.Subscribe(context.Background(), rec, sid, nil); err != nil {
		t.Fatal(err)
	}
	return h, fs, fake, rec, sid
}

// A failed write of the terminal frame loses nothing: the relay reads the run again and the answer arrives once (#9).
func TestStoreFailureKeepsAnswer(t *testing.T) {
	h, fs, _, rec, sid := newTestHub(t)
	var failed atomic.Bool
	fs.failAppend = func(evs []store.NewEvent) bool {
		for _, e := range evs {
			if e.Kind == protocol.KindRunCompleted {
				return failed.CompareAndSwap(false, true)
			}
		}
		return false
	}
	if err := h.Prompt(context.Background(), 1, "s21", "6f1c2a9e-1b7d-4c1e-9a35-000000000009", sid, "what is 2+2?", nil, func(string, bool) {}); err != nil {
		t.Fatal(err)
	}
	got := rec.until(t, isRunEnd)
	if !failed.Load() {
		t.Fatal("the store never failed the terminal frame")
	}
	if end := got[len(got)-1]; end["status"] != protocol.StatusCompleted {
		t.Fatalf("run ended %v, want completed", end["status"])
	}
	answers, completed := 0, 0
	for _, m := range got {
		switch m["kind"] {
		case protocol.KindAnswer:
			answers++
			if text := m["data"].(map[string]any)["text"]; text != "2 + 2 is 4." {
				t.Fatalf("answer = %v", text)
			}
		case protocol.KindRunCompleted:
			completed++
		}
	}
	if answers != 1 || completed != 1 {
		t.Fatalf("got %d answers and %d run.completed, want 1 each: %v", answers, completed, got)
	}
	select {
	case m := <-rec.msgs:
		t.Fatalf("unexpected message after the run ended: %v", m)
	case <-time.After(200 * time.Millisecond):
	}
}

// A resend racing the original prompt gets the same run, never busy: here the second database read finds no row
// but only returns once the first prompt has stored its run and left inflight (#7).
func TestConcurrentResendSameRun(t *testing.T) {
	h, fs, fake, rec, sid := newTestHub(t)
	firstDone := make(chan struct{})
	var once sync.Once
	var lookups atomic.Int32
	fs.afterLookup = func() {
		if lookups.Add(1) == 2 {
			<-firstDone
		}
	}
	type result struct {
		runID    string
		replayed bool
		err      error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			var r result
			r.err = h.Prompt(context.Background(), 1, "s21", "6f1c2a9e-1b7d-4c1e-9a35-000000000007", sid, "what is 2+2?", nil,
				func(id string, replayed bool) { r.runID, r.replayed = id, replayed })
			once.Do(func() { close(firstDone) })
			results <- r
		}()
	}
	a, b := <-results, <-results
	if a.err != nil || b.err != nil {
		t.Fatalf("errors: %v, %v", a.err, b.err)
	}
	if a.runID == "" || a.runID != b.runID {
		t.Fatalf("run ids %q and %q, want the same", a.runID, b.runID)
	}
	if a.replayed == b.replayed {
		t.Fatalf("replayed %v and %v, want exactly one original", a.replayed, b.replayed)
	}
	if creates, _, _, _, _ := fake.Snapshot(); creates != 1 {
		t.Fatalf("Hermes got %d CreateRun calls, want 1", creates)
	}
	// The prompt is announced once: the resend's ack is replayed and adds no user.message (#30).
	announced := 0
	for _, m := range rec.until(t, isRunEnd) {
		if m["kind"] == protocol.KindUserMessage {
			announced++
			if d := m["data"].(map[string]any); d["device"] != "s21" || d["text"] != "what is 2+2?" || d["kind"] != "prompt" {
				t.Fatalf("user.message data %v", d)
			}
		}
	}
	if announced != 1 {
		t.Fatalf("%d user.message events, want 1", announced)
	}
}
