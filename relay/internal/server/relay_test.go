package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/xMuramasa/trismegistus/relay/internal/fakehermes"
	"github.com/xMuramasa/trismegistus/relay/internal/pairing"
	"github.com/xMuramasa/trismegistus/relay/internal/server"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

const approvalCommand = "rm -rf ~/.hermes/cache/scratch/trismegistus-probe-target"

func init() { secret(approvalCommand, "approval command") }

// --- pairing and devices ---

func TestPairCodeWorksOnce(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	code, hash := pairing.NewCode()
	secret(code, "pairing code")
	if _, err := r.st.CreatePairCode(context.Background(), "pixel-8", hash, pairing.CodeTTL, false, 5); err != nil {
		t.Fatal(err)
	}
	// Typed codes are normalized: lower case, no dashes.
	status, body := r.do("POST", "/api/pair", "", map[string]any{"code": strings.ToLower(strings.ReplaceAll(code, "-", "")), "name": "ignored"})
	if status != 200 {
		t.Fatalf("pair: %d %s", status, body)
	}
	var out struct {
		Token  string            `json:"token"`
		Device map[string]string `json:"device"`
	}
	_ = json.Unmarshal(body, &out)
	secret(out.Token, "device token")
	if !pairing.WellFormedToken(out.Token) || out.Device["name"] != "pixel-8" {
		t.Fatalf("bad pair answer %s", body)
	}
	if status, _ := r.do("GET", "/api/sessions", out.Token, nil); status != 200 {
		t.Fatalf("token does not work: %d", status)
	}
	// Used code.
	if status, body := r.do("POST", "/api/pair", "", map[string]any{"code": code}); status != 401 || !strings.Contains(string(body), `"unauthorized"`) {
		t.Fatalf("reused code: %d %s", status, body)
	}
	// Expired code.
	code2, hash2 := pairing.NewCode()
	secret(code2, "pairing code")
	if _, err := r.st.CreatePairCode(context.Background(), "tablet", hash2, -time.Second, false, 5); err != nil {
		t.Fatal(err)
	}
	if status, _ := r.do("POST", "/api/pair", "", map[string]any{"code": code2}); status != 401 {
		t.Fatalf("expired code: %d", status)
	}
	// Malformed and unknown codes, extra fields.
	for _, body := range []any{map[string]any{"code": "nope"}, map[string]any{"code": "AAAA-AAAA-AAAA"}, `{"code":"x","admin":true}`, `[]`} {
		if status, _ := r.do("POST", "/api/pair", "", body); status != 401 && status != 400 {
			t.Fatalf("bad pairing body %v: %d", body, status)
		}
	}
}

func TestPairRateLimit(t *testing.T) {
	opts := fastServerOptions()
	opts.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	r := startRelay(t, newFake(t, "plain"), t.TempDir(), opts, fastHubOptions())
	try := func(xff string) int {
		status, _ := r.doHeaders("POST", "/api/pair", "", map[string]any{"code": "AAAA-AAAA-AAAA"}, map[string]string{"X-Forwarded-For": xff})
		return status
	}
	for i := 0; i < 10; i++ {
		if s := try("203.0.113.7"); s != 401 {
			t.Fatalf("attempt %d: %d", i+1, s)
		}
	}
	if s := try("203.0.113.7"); s != 429 {
		t.Fatalf("11th attempt from one client: %d, want 429", s)
	}
	// The proxy is trusted, so another client behind it has its own budget; a spoofed hop to the left of the
	// real client changes nothing.
	if s := try("203.0.113.8"); s != 401 {
		t.Fatalf("other client: %d", s)
	}
	if s := try("10.9.9.9, 203.0.113.7"); s != 429 {
		t.Fatalf("spoofed left hop: %d, want 429", s)
	}
}

func TestPairRateLimitIgnoresUntrustedXFF(t *testing.T) {
	r := newRelay(t, newFake(t, "plain")) // no trusted proxies
	for i := 0; i < 10; i++ {
		r.doHeaders("POST", "/api/pair", "", map[string]any{"code": "AAAA-AAAA-AAAA"}, map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", i)})
	}
	status, _ := r.doHeaders("POST", "/api/pair", "", map[string]any{"code": "AAAA-AAAA-AAAA"}, map[string]string{"X-Forwarded-For": "198.51.100.99"})
	if status != 429 {
		t.Fatalf("X-Forwarded-For from an untrusted peer must be ignored: %d", status)
	}
}

func TestRevokeDisconnectsLiveSocket(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	token := r.pair("pixel-8")
	other := r.connect(r.pair("tablet"))
	cl := r.connect(token)
	// Revoke from another connection to the same file, as `devices revoke` does from its own process.
	cli, err := store.Open(filepath.Join(r.dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := cli.RevokeDevice(context.Background(), "pixel-8"); err != nil {
		t.Fatal(err)
	}
	if code := cl.closed(); code != websocket.StatusPolicyViolation {
		t.Fatalf("revoked socket closed with %v", code)
	}
	if status, _ := r.do("GET", "/api/sessions", token, nil); status != 401 {
		t.Fatalf("revoked token still works over REST: %d", status)
	}
	other.send(map[string]any{"type": "ping"})
	other.until(isType("pong"))
}

func TestSocketsPerDeviceLimit(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	token := r.pair("pixel-8")
	first := r.connect(token)
	r.connect(token)
	r.connect(token)
	r.connect(token) // the 4th closes the oldest
	if code := first.closed(); code != websocket.StatusPolicyViolation {
		t.Fatalf("oldest socket closed with %v", code)
	}
}

// --- WebSocket auth and limits ---

func TestWSAuthTimeout(t *testing.T) {
	if d := server.DefaultOptions().AuthTimeout; d != 5*time.Second {
		t.Fatalf("default auth timeout %v", d)
	}
	r := newRelay(t, newFake(t, "plain"))
	c := r.dialRaw()
	start := time.Now()
	_, _, err := c.Read(context.Background())
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("expected policy violation close, got %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("closed before the auth timeout")
	}
}

func TestWSBadToken(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	for _, first := range []map[string]any{
		{"type": "auth", "token": secret("trs_"+strings.Repeat("A", 43), "device token")},
		{"type": "auth", "token": "not a token"},
		{"type": "ping"},
	} {
		c := r.dialRaw()
		b, _ := json.Marshal(first)
		if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
		_, data, err := c.Read(context.Background())
		if err != nil {
			t.Fatalf("expected an error message first: %v", err)
		}
		var m msg
		_ = json.Unmarshal(data, &m)
		if m.str("type") != "error" || m.str("code") != "unauthorized" {
			t.Fatalf("got %s", data)
		}
		if err := validator.ValidateDetail(data); err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.Read(context.Background()); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("expected close after unauthorized, got %v", err)
		}
	}
}

func TestWSOversizeMessageCloses(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	cl := r.connect(r.pair("pixel-8"))
	big := strings.Repeat("x", 65*1024)
	_ = cl.c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"prompt","text":"`+big+`"}`))
	if code := cl.closed(); code != websocket.StatusMessageTooBig {
		t.Fatalf("closed with %v, want 1009", code)
	}
}

func TestWSInvalidMessages(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	cl := r.connect(r.pair("pixel-8"))
	cases := map[string]string{
		`{"type":"approve","run_id":"run_1","request_id":"r1","choice":"always"}`:                                   "bad_request",
		`{"type":"prompt","client_msg_id":"c1","session_id":"s1","text":"` + strings.Repeat("y", 32769) + `"}`:      "too_large",
		`{"type":"subscribe","session_id":"../v1/runs"}`:                                                            "bad_request",
		`{"type":"auth","token":"trs_x"}`:                                                                           "bad_request",
		`not json`:                                                                                                  "bad_request",
		`{"type":"ready","device":"x","server_version":"1"}`:                                                        "bad_request",
		`{"type":"stop","run_id":"run_unknown"}`:                                                                    "not_found",
		`{"type":"approve","run_id":"run_unknown","request_id":"a990e19b6d5049798852410f79f88255","choice":"once"}`: "not_found",
	}
	for raw, code := range cases {
		_ = cl.c.Write(context.Background(), websocket.MessageText, []byte(raw))
		m := cl.next()
		if m.str("type") != "error" || m.str("code") != code {
			t.Errorf("%.60s: got %v, want %s", raw, m, code)
		}
	}
}

func TestPromptRateLimit(t *testing.T) {
	opts := fastServerOptions()
	opts.PromptsPerMinute = 2
	fake := newFake(t, "plain")
	r := startRelay(t, fake, t.TempDir(), opts, fastHubOptions())
	cl := r.connect(r.pair("pixel-8"))
	sid := fake.AddSession("api_server")
	cl.subscribe(sid)
	for i := 0; i < 2; i++ {
		if _, ack := cl.prompt(sid); ack.str("type") != "ack" {
			t.Fatalf("prompt %d: %v", i, ack)
		}
		cl.until(isTerminal)
	}
	if _, m := cl.prompt(sid); m.str("code") != "rate_limited" {
		t.Fatalf("3rd prompt: %v", m)
	}
}

// --- runs from the recorded fixtures ---

func TestFixtureRuns(t *testing.T) {
	cases := []struct {
		fixture string
		kinds   string
		answer  string
		status  string
	}{
		{"plain", "answer run.completed", "2 + 2 is 4.", "completed"},
		{"tool", "tool.started tool.completed answer run.completed", "The terminal printed 2026.", "completed"},
		{"subagent", "tool.started subagent.started tool.completed subagent.completed answer run.completed",
			"Subagent dispatched (sa-0-31a5bc12); I'll report its reply when it comes back.", "completed"},
		{"approval", "tool.started approval.requested approval.resolved tool.completed answer run.completed",
			"The command was denied and did not run, so I stopped without retrying.", "completed"},
		{"stop", "run.cancelled", "", "cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			fake := newFake(t, tc.fixture)
			r := newRelay(t, fake)
			a := r.connect(r.pair("pixel-8"))
			b := r.connect(r.pair("tablet"))
			sid := fake.AddSession("api_server")
			a.subscribe(sid)
			b.subscribe(sid)
			_, ack := a.prompt(sid)
			if ack.str("type") != "ack" || ack["replayed"] != false {
				t.Fatalf("ack: %v", ack)
			}
			runID := ack.str("run_id")
			var got []msg
			switch tc.fixture {
			case "approval":
				got = a.until(func(m msg) bool { return m.str("kind") == "approval.requested" })
				d := got[len(got)-1].data()
				if fmt.Sprint(d["choices"]) != "[once session deny]" || !strings.HasPrefix(d.str("command"), approvalCommand) {
					t.Fatalf("approval data: %v", d)
				}
				a.send(map[string]any{"type": "approve", "run_id": runID, "request_id": d.str("request_id"), "choice": "deny"})
			case "stop":
				a.send(map[string]any{"type": "stop", "run_id": runID})
			}
			got = append(got, a.until(isTerminal)...)
			evs := events(got)
			if kinds(evs) != tc.kinds {
				t.Fatalf("kinds: %q, want %q", kinds(evs), tc.kinds)
			}
			checkSeqs(t, evs, 1)
			for _, e := range evs {
				if e.str("run_id") != runID || e.str("session_id") != sid {
					t.Fatalf("event for another run: %v", e)
				}
				for _, k := range []string{"usage", "cost_usd", "input_tokens"} {
					if _, ok := e.data()[k]; ok {
						t.Fatalf("event data leaks %s: %v", k, e)
					}
				}
				if e.str("kind") == "answer" && e.data().str("text") != tc.answer {
					t.Fatalf("answer %q", e.data().str("text"))
				}
			}
			// The second device sees the same events and the run's status changes.
			gotB := b.until(isRunEnd)
			if kinds(events(gotB)) != tc.kinds {
				t.Fatalf("second device kinds: %q", kinds(events(gotB)))
			}
			checkSeqs(t, events(gotB), 1)
			var statuses []string
			for _, m := range gotB {
				if m.str("type") == "run" {
					statuses = append(statuses, m.str("status"))
				}
			}
			want := "running " + tc.status
			if tc.fixture == "approval" {
				want = "running waiting_for_approval running completed"
			}
			if strings.Join(statuses, " ") != want {
				t.Fatalf("run statuses %v, want %s", statuses, want)
			}
			if tc.fixture == "tool" {
				d := evs[1].data()
				if d["duration"] != 0.15 || d["error"] != false || d.str("tool") != "terminal" {
					t.Fatalf("tool.completed data %v", d)
				}
			}
			_, approvals, stops, _, _ := fake.Snapshot()
			switch tc.fixture {
			case "approval":
				if len(approvals) != 1 || approvals[0].RequestID != "a990e19b6d5049798852410f79f88255" || approvals[0].Choice != "deny" ||
					approvals[0].RunID != runID || !approvals[0].HasGeneration {
					t.Fatalf("approval calls %+v", approvals)
				}
			case "stop":
				if len(stops) != 1 || stops[0] != runID {
					t.Fatalf("stop calls %v", stops)
				}
			}
			a.until(isRunEnd)
			a.quiet(100 * time.Millisecond)
		})
	}
}

func TestApproveOnceAndSession(t *testing.T) {
	for _, choice := range []string{"once", "session"} {
		t.Run(choice, func(t *testing.T) {
			fake := newFake(t, "approval")
			r := newRelay(t, fake)
			a := r.connect(r.pair("pixel-8"))
			sid := fake.AddSession("api_server")
			a.subscribe(sid)
			_, ack := a.prompt(sid)
			got := a.until(func(m msg) bool { return m.str("kind") == "approval.requested" })
			rid := got[len(got)-1].data().str("request_id")
			a.send(map[string]any{"type": "approve", "run_id": ack.str("run_id"), "request_id": rid, "choice": choice})
			a.until(isTerminal)
			_, approvals, _, _, _ := fake.Snapshot()
			if len(approvals) != 1 || approvals[0].Choice != choice || approvals[0].RequestID != rid {
				t.Fatalf("approval calls %+v", approvals)
			}
		})
	}
}

// A device that subscribes while a run waits for approval gets the request (#18): without after_seq the pending
// approval.requested event comes again, with its original seq, before the run status, and the device can answer it.
// A subscribe whose after_seq already covers it gets no repeat.
func TestLateSubscriberGetsPendingApproval(t *testing.T) {
	fake := newFake(t, "approval")
	r := newRelay(t, fake)
	a := r.connect(r.pair("pixel-8"))
	sid := fake.AddSession("api_server")
	a.subscribe(sid)
	_, ack := a.prompt(sid)
	runID := ack.str("run_id")
	got := a.until(func(m msg) bool { return m.str("type") == "run" && m.str("status") == "waiting_for_approval" })
	var req msg
	for _, m := range events(got) {
		if m.str("kind") == "approval.requested" {
			req = m
		}
	}
	if req == nil {
		t.Fatalf("no approval.requested before the wait: %v", got)
	}
	rid := req.data().str("request_id")

	late := r.connect(r.pair("macbook"))
	late.send(map[string]any{"type": "subscribe", "session_id": sid})
	late.send(map[string]any{"type": "ping"})
	got = late.until(isType("pong"))
	if len(got) != 3 {
		t.Fatalf("late subscriber got %v, want the pending request, the run status, pong", got)
	}
	if m := got[0]; m.str("kind") != "approval.requested" || m.num("seq") != req.num("seq") ||
		m.str("run_id") != runID || m.data().str("request_id") != rid || m.data().str("command") != req.data().str("command") {
		t.Fatalf("late subscriber, first message %v, want the pending request %v", m, req)
	}
	if m := got[1]; m.str("type") != "run" || m.str("status") != "waiting_for_approval" {
		t.Fatalf("late subscriber, second message %v", m)
	}

	caughtUp := r.connect(r.pair("tablet"))
	caughtUp.send(map[string]any{"type": "subscribe", "session_id": sid, "after_seq": req.num("seq")})
	caughtUp.send(map[string]any{"type": "ping"})
	if got := caughtUp.until(isType("pong")); len(got) != 2 || got[0].str("type") != "run" || got[0].str("status") != "waiting_for_approval" {
		t.Fatalf("subscriber with after_seq got %v, want the run status and pong (the request must not repeat)", got)
	}

	late.send(map[string]any{"type": "approve", "run_id": runID, "request_id": rid, "choice": "deny"})
	late.until(isTerminal)
	if _, approvals, _, _, _ := fake.Snapshot(); len(approvals) != 1 || approvals[0].RequestID != rid || approvals[0].Choice != "deny" {
		t.Fatalf("approval calls %+v", approvals)
	}
}

func TestOnlySubscribersControlRuns(t *testing.T) {
	fake := newFake(t, "approval")
	r := newRelay(t, fake)
	a := r.connect(r.pair("pixel-8"))
	outsider := r.connect(r.pair("tablet"))
	sid := fake.AddSession("api_server")
	a.subscribe(sid)
	_, ack := a.prompt(sid)
	got := a.until(func(m msg) bool { return m.str("kind") == "approval.requested" })
	rid := got[len(got)-1].data().str("request_id")
	runID := ack.str("run_id")
	for _, m := range []map[string]any{
		{"type": "approve", "run_id": runID, "request_id": rid, "choice": "once"},
		{"type": "stop", "run_id": runID},
		{"type": "steer", "run_id": runID, "text": "go faster"},
	} {
		outsider.send(m)
		if e := outsider.next(); e.str("code") != "not_found" || e.str("ref") != runID {
			t.Fatalf("outsider %s: %v", m["type"], e)
		}
	}
	if _, approvals, stops, steers, _ := fake.Snapshot(); len(approvals)+len(stops)+len(steers) != 0 {
		t.Fatal("an outsider's control reached Hermes")
	}
	a.send(map[string]any{"type": "approve", "run_id": runID, "request_id": rid, "choice": "deny"})
	a.until(isTerminal)
}

func TestStopAndSteerReachHermes(t *testing.T) {
	fake := newFake(t, "stop")
	r := newRelay(t, fake)
	a := r.connect(r.pair("pixel-8"))
	sid := fake.AddSession("api_server")
	a.subscribe(sid)
	_, ack := a.prompt(sid)
	runID := ack.str("run_id")
	steer := secret("secret steer text: shorter please", "steer text")
	a.send(map[string]any{"type": "steer", "run_id": runID, "text": steer})
	got := a.until(func(m msg) bool { return m.str("kind") == "steer.queued" || m.str("type") == "error" })
	if got[len(got)-1].str("kind") != "steer.queued" {
		t.Fatalf("steer: %v", got)
	}
	a.send(map[string]any{"type": "stop", "run_id": runID})
	evs := events(append(got, a.until(isTerminal)...))
	if kinds(evs) != "steer.queued run.cancelled" {
		t.Fatalf("kinds %q", kinds(evs))
	}
	checkSeqs(t, evs, 1)
	if d := evs[1].data(); d.str("reason") != "interrupted_during_api_call" {
		t.Fatalf("cancel data %v", d)
	}
	_, _, stops, steers, _ := fake.Snapshot()
	if len(stops) != 1 || len(steers) != 1 || steers[0] != steer {
		t.Fatalf("stops %v steers %d", stops, len(steers))
	}
	a.until(isRunEnd)
	// Steering a finished run: not active any more.
	a.send(map[string]any{"type": "steer", "run_id": runID, "text": "too late"})
	if e := a.next(); e.str("code") != "not_found" {
		t.Fatalf("steer after end: %v", e)
	}
}

// --- prompts: idempotency, busy, read-only, Hermes down ---

func TestIdempotentPromptAndBusy(t *testing.T) {
	fake := newFake(t, "plain")
	hold := make(chan struct{})
	r := newRelay(t, fake)
	a := r.connect(r.pair("pixel-8"))
	sid := fake.AddSession("api_server")
	fake.SetScript(sid, fakehermes.Script{Fixture: fixture(t, "plain"), Hold: map[int]chan struct{}{0: hold}, FromStart: true})
	a.subscribe(sid)

	// A double tap: the same prompt twice, back to back.
	text := secret("secret double tap text", "prompt text")
	p := map[string]any{"type": "prompt", "client_msg_id": "0b9e3c1e-8a6f-4d3b-9f7e-1d2c3b4a5f60", "session_id": sid, "text": text}
	a.send(p)
	a.send(p)
	acks := []msg{}
	for len(acks) < 2 {
		if m := a.next(); m.str("type") == "ack" {
			acks = append(acks, m)
		} else if m.str("type") == "error" {
			t.Fatalf("error %v", m)
		}
	}
	if acks[0].str("run_id") != acks[1].str("run_id") || acks[0]["replayed"] == acks[1]["replayed"] {
		t.Fatalf("acks %v", acks)
	}
	runID := acks[0].str("run_id")

	// Resend while running: replayed, same run.
	a.send(p)
	if m := a.until(isType("ack"))[0]; m.str("run_id") != runID || m["replayed"] != true {
		t.Fatalf("resend ack %v", m)
	}
	// Another prompt while the run is active.
	if _, m := a.prompt(sid); m.str("code") != "busy" {
		t.Fatalf("second prompt: %v", m)
	}
	close(hold)
	a.until(isTerminal)
	// Resend after the end: still the same run, no new turn.
	a.send(p)
	if m := a.until(isType("ack")); m[len(m)-1].str("run_id") != runID || m[len(m)-1]["replayed"] != true {
		t.Fatalf("late resend ack %v", m)
	}
	if creates, _, _, _, _ := fake.Snapshot(); creates != 1 {
		t.Fatalf("Hermes got %d run creations, want 1", creates)
	}
}

func TestReadOnlySessions(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	token := r.pair("pixel-8")
	a := r.connect(token)
	tg := fake.AddSession("telegram")
	a.subscribe(tg)
	if _, m := a.prompt(tg); m.str("code") != "read_only" {
		t.Fatalf("prompt in telegram session: %v", m)
	}
	if creates, _, _, _, _ := fake.Snapshot(); creates != 0 {
		t.Fatal("a read-only prompt reached Hermes")
	}
	if status, _ := r.do("GET", "/api/sessions/"+tg+"/messages", token, nil); status != 200 {
		t.Fatalf("reading a telegram session: %d", status)
	}
	if status, body := r.do("PATCH", "/api/sessions/"+tg, token, map[string]any{"title": "mine"}); status != 403 || !strings.Contains(string(body), "read_only") {
		t.Fatalf("rename telegram session: %d %s", status, body)
	}
	if status, _ := r.do("DELETE", "/api/sessions/"+tg, token, nil); status != 403 {
		t.Fatalf("delete telegram session: %d", status)
	}
	if status, _ := r.do("GET", "/api/sessions/"+tg, token, nil); status != 200 {
		t.Fatal("telegram session vanished")
	}
	// Unknown session.
	if _, m := a.prompt("api_404_nope"); m.str("code") != "not_found" {
		t.Fatalf("prompt in unknown session: %v", m)
	}
}

func TestHermesUnavailable(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	a := r.connect(r.pair("pixel-8"))
	sid := fake.AddSession("api_server")
	a.subscribe(sid) // caches the source before Hermes goes down
	if _, m := a.prompt(sid); m.str("type") != "ack" {
		t.Fatal(m)
	}
	a.until(isTerminal)
	fake.FailCreateRun(503, "7")
	_, m := a.prompt(sid)
	if m.str("code") != "hermes_unavailable" || !strings.Contains(m.str("message"), "retry after 7s") {
		t.Fatalf("got %v", m)
	}
	// Not busy afterwards: the failed start released the session.
	fake.FailCreateRun(0, "")
	if _, m := a.prompt(sid); m.str("type") != "ack" {
		t.Fatalf("after recovery: %v", m)
	}
}

// --- reconnect and replay ---

func TestSubscribeReplay(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	token := r.pair("pixel-8")
	a := r.connect(token)
	sid := fake.AddSession("api_server")
	a.subscribe(sid)
	a.prompt(sid)
	a.until(isTerminal) // seq 1-2
	fake.SetScript(sid, fakehermes.Script{Fixture: fixture(t, "tool"), FromStart: true})
	a.prompt(sid)
	a.until(isTerminal) // seq 3-6

	b := r.connect(token)
	b.send(map[string]any{"type": "subscribe", "session_id": sid, "after_seq": 2})
	got := events(b.until(func(m msg) bool { return m.num("seq") == 6 }))
	if kinds(got) != "tool.started tool.completed answer run.completed" {
		t.Fatalf("replay kinds %q", kinds(got))
	}
	checkSeqs(t, got, 3)
	b.quiet(100 * time.Millisecond)

	c := r.connect(token)
	c.send(map[string]any{"type": "subscribe", "session_id": sid, "after_seq": 0})
	checkSeqs(t, events(c.until(func(m msg) bool { return m.num("seq") == 6 })), 1)

	d := r.connect(token)
	d.send(map[string]any{"type": "subscribe", "session_id": sid, "after_seq": 6})
	d.quiet(100 * time.Millisecond)

	// Beyond the last seq (the relay's database was reset): replay_gap.
	d.send(map[string]any{"type": "subscribe", "session_id": sid, "after_seq": 99})
	if m := d.next(); m.str("code") != "replay_gap" || m.str("ref") != sid {
		t.Fatalf("got %v", m)
	}
	// Older than the 24 h buffer.
	r.st.SetClock(func() time.Time { return time.Now().Add(25 * time.Hour) })
	if _, err := r.st.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.send(map[string]any{"type": "subscribe", "session_id": sid, "after_seq": 2})
	if m := d.next(); m.str("code") != "replay_gap" {
		t.Fatalf("got %v", m)
	}
	// Numbering continues after the pruning, and a fresh subscribe works.
	d.subscribe(sid)
	r.st.SetClock(time.Now)
	fake.SetScript(sid, fakehermes.Script{Fixture: fixture(t, "plain"), FromStart: true})
	d.prompt(sid)
	checkSeqs(t, events(d.until(isTerminal)), 7)
}

func TestStreamCutReattachesWithoutDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script fakehermes.Script
		kinds  string
	}{
		{"cut, server replays from start", fakehermes.Script{CutAfter: []int{1}, FromStart: true},
			"tool.started tool.completed reconnected answer run.completed"},
		{"cut, server continues", fakehermes.Script{CutAfter: []int{0}},
			"tool.started reconnected tool.completed answer run.completed"},
		{"two cuts", fakehermes.Script{CutAfter: []int{0, 2}, FromStart: true},
			"tool.started reconnected tool.completed answer reconnected run.completed"},
		{"silent stream", fakehermes.Script{StallAfter: []int{1}, FromStart: true},
			"tool.started tool.completed reconnected answer run.completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake(t, "tool")
			r := newRelay(t, fake)
			a := r.connect(r.pair("pixel-8"))
			sid := fake.AddSession("api_server")
			tc.script.Fixture = fixture(t, "tool")
			if tc.name == "two cuts" {
				// Cut between reasoning.available (dropped) and run.completed: frame 2 is reasoning.
				tc.kinds = "tool.started reconnected tool.completed reconnected answer run.completed"
			}
			fake.SetScript(sid, tc.script)
			a.subscribe(sid)
			_, ack := a.prompt(sid)
			evs := events(a.until(isTerminal))
			if kinds(evs) != tc.kinds {
				t.Fatalf("kinds %q, want %q", kinds(evs), tc.kinds)
			}
			checkSeqs(t, evs, 1)
			if run := fake.Run(ack.str("run_id")); run.Streams < 2 {
				t.Fatalf("streams %d", run.Streams)
			}
		})
	}
}

func TestTerminalFromRunStatus(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	a := r.connect(r.pair("pixel-8"))
	sid := fake.AddSession("api_server")
	hold := make(chan struct{})
	fake.SetScript(sid, fakehermes.Script{Fixture: fixture(t, "plain"), Hold: map[int]chan struct{}{0: hold}, CutAfter: []int{0}})
	a.subscribe(sid)
	_, ack := a.prompt(sid)
	fake.WithRun(ack.str("run_id"), func(run *fakehermes.Run) {
		run.EventsGone = true
		run.StatusOverride = "completed"
	})
	close(hold)
	evs := events(a.until(isTerminal))
	if kinds(evs) != "reconnected answer run.completed" || evs[1].data().str("text") != "2 + 2 is 4." {
		t.Fatalf("got %q %v", kinds(evs), evs)
	}
}

func TestRelayRestartMidRun(t *testing.T) {
	fake := newFake(t, "tool")
	r := newRelay(t, fake)
	token := r.pair("pixel-8")
	a := r.connect(token)
	sid := fake.AddSession("api_server")
	hold := make(chan struct{})
	fake.SetScript(sid, fakehermes.Script{Fixture: fixture(t, "tool"), Hold: map[int]chan struct{}{2: hold}, FromStart: true})
	a.subscribe(sid)
	_, ack := a.prompt(sid)
	before := events(a.until(func(m msg) bool { return m.str("kind") == "tool.completed" }))
	checkSeqs(t, before, 1)

	r2 := r.restart()
	if code := a.closed(); code != websocket.StatusGoingAway {
		t.Fatalf("socket closed with %v on shutdown, want going away", code)
	}
	b := r2.connect(token)
	b.send(map[string]any{"type": "subscribe", "session_id": sid, "after_seq": 2})
	close(hold)
	after := events(b.until(isTerminal))
	if kinds(after) != "reconnected answer run.completed" {
		t.Fatalf("after restart %q", kinds(after))
	}
	checkSeqs(t, after, 3)
	for _, e := range after {
		if e.str("run_id") != ack.str("run_id") {
			t.Fatalf("wrong run %v", e)
		}
	}
	if e := after[0].data(); e.str("reason") != "relay_restart" {
		t.Fatalf("reconnected data %v", e)
	}
	// The session is free again.
	fake.SetScript(sid, fakehermes.Script{Fixture: fixture(t, "plain"), FromStart: true})
	if _, m := b.prompt(sid); m.str("type") != "ack" {
		t.Fatalf("prompt after restart: %v", m)
	}
	checkSeqs(t, events(b.until(isTerminal)), 6)
}

func TestRunForgottenByHermesFails(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	a := r.connect(r.pair("pixel-8"))
	sid := fake.AddSession("api_server")
	hold := make(chan struct{})
	fake.SetScript(sid, fakehermes.Script{Fixture: fixture(t, "plain"), Hold: map[int]chan struct{}{0: hold}, CutAfter: []int{0}})
	a.subscribe(sid)
	_, ack := a.prompt(sid)
	// Hermes restarts and forgets the run: the stream and the status both 404.
	fake.ForgetRun(ack.str("run_id"))
	close(hold)
	evs := events(a.until(isTerminal))
	if kinds(evs) != "reconnected run.failed" {
		t.Fatalf("got %q", kinds(evs))
	}
}

func TestRunGivesUpWhenHermesStaysDown(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake) // GiveUp is 3 s in tests (2 h in production)
	a := r.connect(r.pair("pixel-8"))
	sid := fake.AddSession("api_server")
	hold := make(chan struct{})
	defer close(hold)
	fake.SetScript(sid, fakehermes.Script{Fixture: fixture(t, "plain"), Hold: map[int]chan struct{}{0: hold}})
	a.subscribe(sid)
	a.prompt(sid)
	fake.Close()
	evs := events(a.until(isTerminal))
	if kinds(evs) != "reconnected run.failed" || evs[1].data().str("error") != "lost contact with Hermes" {
		t.Fatalf("got %q %v", kinds(evs), evs)
	}
}

// --- REST ---

func TestRESTAllowlist(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	token := r.pair("pixel-8")
	sid := fake.AddSession("api_server")

	if status, body := r.do("GET", "/healthz", "", nil); status != 200 || string(body) != "ok\n" {
		t.Fatalf("healthz %d %q", status, body)
	}
	for _, tok := range []string{"", "trs_" + strings.Repeat("B", 43), fakeKey} {
		if status, _ := r.do("GET", "/api/sessions", tok, nil); status != 401 {
			t.Fatalf("token %q: %d", tok, status)
		}
	}
	status, body := r.do("GET", "/api/sessions?limit=5&secret=1", token, nil)
	if status != 200 || !strings.Contains(string(body), sid) {
		t.Fatalf("list %d %s", status, body)
	}
	status, body = r.do("POST", "/api/sessions", token, map[string]any{"title": "a title Hermes would reject"})
	if status != 201 {
		t.Fatalf("create %d %s", status, body)
	}
	if posts := fake.SessionPost; len(posts) != 1 || len(posts[0]) != 0 {
		t.Fatalf("relay sent a title: %v", posts)
	}
	var created struct {
		Session struct{ ID string } `json:"session"`
	}
	_ = json.Unmarshal(body, &created)
	if status, _ := r.do("GET", "/api/sessions/"+created.Session.ID, token, nil); status != 200 {
		t.Fatalf("get %d", status)
	}
	if status, _ := r.do("GET", "/api/sessions/"+sid+"/messages?limit=50", token, nil); status != 200 {
		t.Fatalf("messages %d", status)
	}
	if status, _ := r.do("PATCH", "/api/sessions/"+sid, token, map[string]any{"title": "Renamed"}); status != 200 {
		t.Fatalf("rename %d", status)
	}
	if status, _ := r.do("PATCH", "/api/sessions/"+sid, token, map[string]any{"title": ""}); status != 400 {
		t.Fatalf("empty title %d", status)
	}
	if status, _ := r.do("DELETE", "/api/sessions/"+created.Session.ID, token, nil); status != 200 {
		t.Fatalf("delete %d", status)
	}
	if status, _ := r.do("GET", "/api/sessions/api_404_nope", token, nil); status != 404 {
		t.Fatalf("unknown session %d", status)
	}

	_, _, _, _, before := fake.Snapshot()
	for _, p := range []string{"/v1/runs", "/v1/capabilities", "/api/skills", "/api/model/options", "/api/sessions/" + sid + "/chat/stream",
		"/api/sessions/" + sid + "/messages/1", "/api/sessions/a%2Fb", "/api/sessions/..%2Fv1", "/admin", "/api/jobs"} {
		for _, method := range []string{"GET", "POST"} {
			if status, _ := r.do(method, p, token, map[string]any{}); status != 404 && status != 405 {
				t.Errorf("%s %s: %d, want 404", method, p, status)
			}
		}
	}
	if _, _, _, _, after := fake.Snapshot(); len(after) != len(before) {
		t.Fatalf("unknown routes reached Hermes: %v", after[len(before):])
	}
	for _, p := range fake.Paths {
		if strings.Contains(p, "secret") {
			t.Fatalf("a non-allowlisted query parameter reached Hermes: %s", p)
		}
	}
}

func TestRESTHermesDown(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	token := r.pair("pixel-8")
	fake.Close()
	status, body := r.do("GET", "/api/sessions", token, nil)
	if status != 503 || !strings.Contains(string(body), "hermes_unavailable") {
		t.Fatalf("got %d %s", status, body)
	}
}

func TestLogsHaveNoSecrets(t *testing.T) {
	// The whole suite's logs are checked in TestMain; this checks a full approval flow right away.
	fake := newFake(t, "approval")
	r := newRelay(t, fake)
	a := r.connect(r.pair("pixel-8"))
	sid := fake.AddSession("api_server")
	a.subscribe(sid)
	_, ack := a.prompt(sid)
	got := a.until(func(m msg) bool { return m.str("kind") == "approval.requested" })
	a.send(map[string]any{"type": "steer", "run_id": ack.str("run_id"), "text": secret("secret steer while waiting", "steer text")})
	a.send(map[string]any{"type": "approve", "run_id": ack.str("run_id"), "request_id": got[len(got)-1].data().str("request_id"), "choice": "deny"})
	a.until(isTerminal)
	logs := suiteLogs.String()
	if !strings.Contains(logs, `"run finished"`) {
		t.Fatal("expected run logs")
	}
	secretsMu.Lock()
	defer secretsMu.Unlock()
	for s, what := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain a %s", what)
		}
	}
}
