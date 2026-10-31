// Package fakehermes is a test double of the Hermes API server. It implements the endpoints the relay uses and
// replays the phase 0 recordings (probe/fixtures/*.jsonl) with controllable timing: hold a frame until released,
// cut the stream after a frame, go silent after a frame, or replay from the start on re-attach.
package fakehermes

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"
)

// Fixture is one recorded scenario.
type Fixture struct {
	Name    string
	Frames  []map[string]any // SSE data frames, in order
	Times   []float64        // recording time of each frame
	Final   map[string]any   // GET /v1/runs/{id} once the run ended
	Pending map[string]any   // GET /v1/runs/{id} while waiting for approval
	// GateAt is the index of the frame that waits for a control call (GateOn: "approval" or "stop"); -1 for none.
	GateAt int
	GateOn string
}

// LoadFixture reads probe/fixtures/<name>.jsonl.
func LoadFixture(path string) (*Fixture, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fx := &Fixture{Name: strings.TrimSuffix(path[strings.LastIndexByte(path, '/')+1:], ".jsonl"), GateAt: -1}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var line struct {
			T     float64         `json:"t"`
			Kind  string          `json:"kind"`
			Event *string         `json:"event"`
			Data  json.RawMessage `json:"data"`
			Body  map[string]any  `json:"body"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		switch line.Kind {
		case "sse":
			if line.Event != nil && *line.Event == ":comment" {
				continue
			}
			var frame map[string]any
			if err := json.Unmarshal(line.Data, &frame); err != nil {
				return nil, fmt.Errorf("%s: frame: %w", path, err)
			}
			fx.Frames = append(fx.Frames, frame)
			fx.Times = append(fx.Times, line.T)
		case "run.status.final":
			fx.Final = line.Body
		case "run.status.pending":
			fx.Pending = line.Body
		case "run.approval":
			fx.GateAt, fx.GateOn = len(fx.Frames), "approval"
		case "run.stop":
			fx.GateAt, fx.GateOn = len(fx.Frames), "stop"
		}
	}
	return fx, sc.Err()
}

// Script says how runs started in a session behave.
type Script struct {
	Fixture *Fixture
	// Hold makes frame i wait until the channel is closed.
	Hold map[int]chan struct{}
	// CutAfter closes the stream right after frame i (once per index).
	CutAfter []int
	// StallAfter stops sending anything, keepalives included, after frame i, keeping the connection open (once).
	StallAfter []int
	// FromStart replays every frame on each attach (like a server that keeps events); otherwise an attach
	// continues after the last frame sent.
	FromStart bool
}

// Session is a fake Hermes session.
type Session struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Title  any    `json:"title"`
	Pinned bool   `json:"pinned"`
	// ActiveTurn makes DELETE answer 409 session_active_turn, as while another surface's turn is writing.
	ActiveTurn bool `json:"-"`
}

// Run is a fake run.
type Run struct {
	ID, SessionID string
	script        Script
	frames        [][]byte
	gate          chan struct{} // the fixture's control gate
	gateOpen      bool
	cut, stall    map[int]bool
	sent          int // frames sent so far (high-water)
	terminal      bool
	Streams       int // SSE connections opened
	// EventsGone makes the events endpoint answer 404 (Hermes forgot the stream).
	EventsGone bool
	// StatusOverride replaces GET /v1/runs/{id} (e.g. "completed" with the fixture's final body).
	StatusOverride string
}

// ApprovalCall records POST /v1/runs/{id}/approval.
type ApprovalCall struct {
	RunID, RequestID, Choice string
	HasGeneration            bool
}

// Fake is the fake Hermes server.
type Fake struct {
	Key string
	URL string
	srv *httptest.Server

	// KeepAlive is the period of ": keepalive" comments (Hermes: 10 s).
	KeepAlive time.Duration
	// Speed scales the fixtures' recorded timing (0: no delays).
	Speed float64

	mu          sync.Mutex
	sessions    map[string]*Session
	order       []string
	runs        map[string]*Run
	idem        map[string]string
	scripts     map[string]Script
	Default     Script
	next        int
	createFail  int
	retryAfter  string
	CreateCalls int
	RunInputs   []string
	Approvals   []ApprovalCall
	Stops       []string
	Steers      []string
	Paths       []string // every request, "METHOD /path"
	SessionPost []map[string]any
}

// New starts a fake Hermes. Close it with t.Cleanup(f.Close).
func New(key string, def *Fixture) *Fake {
	f := &Fake{
		Key: key, KeepAlive: 50 * time.Millisecond,
		sessions: map[string]*Session{}, runs: map[string]*Run{}, idem: map[string]string{}, scripts: map[string]Script{},
		Default: Script{Fixture: def, FromStart: true},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.URL = f.srv.URL
	return f
}

// Close stops the server.
func (f *Fake) Close() {
	f.srv.CloseClientConnections()
	f.srv.Close()
}

// AddSession creates a session with a source (api_server, telegram...).
func (f *Fake) AddSession(source string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addSessionLocked(source)
}

func (f *Fake) addSessionLocked(source string) string {
	f.next++
	id := fmt.Sprintf("api_%d_%08x", 1792800000+f.next, f.next)
	if source != "api_server" {
		id = fmt.Sprintf("20261023_%06d_%06x", f.next, f.next)
	}
	f.sessions[id] = &Session{ID: id, Source: source}
	f.order = append(f.order, id)
	return id
}

// SetScript sets how runs in a session behave.
func (f *Fake) SetScript(sessionID string, s Script) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[sessionID] = s
}

// FailCreateRun makes POST /v1/runs answer status (0 to stop) with a Retry-After.
func (f *Fake) FailCreateRun(status int, retryAfter string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createFail, f.retryAfter = status, retryAfter
}

// Run returns a run by id.
func (f *Fake) Run(id string) *Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[id]
}

// ForgetRun drops a run, as a restarted Hermes would: its stream and status answer 404.
func (f *Fake) ForgetRun(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.runs, id)
}

// WithRun runs fn with the fake locked.
func (f *Fake) WithRun(id string, fn func(*Run)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.runs[id])
}

// Snapshot returns copies of the recorded calls.
func (f *Fake) Snapshot() (creates int, approvals []ApprovalCall, stops, steers, paths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.CreateCalls, append([]ApprovalCall(nil), f.Approvals...), append([]string(nil), f.Stops...),
		append([]string(nil), f.Steers...), append([]string(nil), f.Paths...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.Paths = append(f.Paths, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.Key {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", f.listSessions)
	mux.HandleFunc("POST /api/sessions", f.createSession)
	mux.HandleFunc("GET /api/sessions/{id}", f.getSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", f.patchSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", f.deleteSession)
	mux.HandleFunc("GET /api/sessions/{id}/messages", f.messages)
	mux.HandleFunc("POST /v1/runs", f.createRun)
	mux.HandleFunc("GET /v1/runs/{id}", f.getRun)
	mux.HandleFunc("GET /v1/runs/{id}/events", f.events)
	mux.HandleFunc("POST /v1/runs/{id}/approval", f.approve)
	mux.HandleFunc("POST /v1/runs/{id}/stop", f.stop)
	mux.HandleFunc("POST /v1/runs/{id}/steer", f.steer)
	mux.ServeHTTP(w, r)
}

func (f *Fake) listSessions(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data := []*Session{}
	for _, id := range f.order {
		if s := f.sessions[id]; s != nil {
			data = append(data, s)
		}
	}
	writeJSON(w, 200, map[string]any{"data": data, "has_more": false})
}

func (f *Fake) createSession(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.SessionPost = append(f.SessionPost, body)
	id := f.addSessionLocked("api_server")
	writeJSON(w, 201, map[string]any{"object": "hermes.session", "session": f.sessions[id]})
}

func (f *Fake) session(w http.ResponseWriter, r *http.Request) *Session {
	s := f.sessions[r.PathValue("id")]
	if s == nil {
		writeJSON(w, 404, map[string]any{"error": "not found"})
	}
	return s
}

func (f *Fake) getSession(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.session(w, r); s != nil {
		writeJSON(w, 200, map[string]any{"object": "hermes.session", "session": s})
	}
}

// SetActiveTurn marks a session as having a turn in progress (see Session.ActiveTurn).
func (f *Fake) SetActiveTurn(id string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id].ActiveTurn = on
}

// patchSession accepts Hermes' keys only; unknown ones are a 400, like the real server.
func (f *Fake) patchSession(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad json"})
		return
	}
	for k := range body {
		switch k {
		case "title", "pinned", "archived", "hidden", "unread", "end_reason":
		default:
			writeJSON(w, 400, map[string]any{"error": "unknown field " + k})
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.session(w, r)
	if s == nil {
		return
	}
	if t, ok := body["title"]; ok {
		s.Title = t
	}
	if p, ok := body["pinned"].(bool); ok {
		s.Pinned = p
	}
	writeJSON(w, 200, map[string]any{"object": "hermes.session", "session": s})
}

func (f *Fake) deleteSession(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.session(w, r)
	if s == nil {
		return
	}
	busy := s.ActiveTurn
	for _, run := range f.runs {
		busy = busy || run.SessionID == s.ID && !run.terminal
	}
	if busy {
		writeJSON(w, 409, map[string]any{"error": map[string]any{"code": "session_active_turn",
			"message": "a turn is in progress in this session"}})
		return
	}
	delete(f.sessions, s.ID)
	writeJSON(w, 200, map[string]any{"object": "hermes.session.deleted", "id": s.ID, "deleted": true})
}

func (f *Fake) messages(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.session(w, r); s != nil {
		writeJSON(w, 200, map[string]any{"object": "list", "session_id": s.ID, "data": []any{},
			"pagination": map[string]any{"limit": 500, "offset": 0, "returned": 0}})
	}
}

func (f *Fake) createRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Input     string `json:"input"`
		SessionID string `json:"session_id"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad json"})
		return
	}
	key := r.Header.Get("Idempotency-Key")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreateCalls++
	if f.createFail != 0 {
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		writeJSON(w, f.createFail, map[string]any{"error": "unavailable"})
		return
	}
	if id, ok := f.idem[key]; ok && key != "" {
		w.Header().Set("Idempotency-Replayed", "true")
		writeJSON(w, 202, map[string]any{"run_id": id, "status": "running", "replayed": true})
		return
	}
	s := f.sessions[body.SessionID]
	if s == nil {
		writeJSON(w, 404, map[string]any{"error": "session not found"})
		return
	}
	if s.Source != "api_server" {
		writeJSON(w, 409, map[string]any{"error": "session owned by another surface"})
		return
	}
	for _, run := range f.runs {
		if run.SessionID == s.ID && !run.terminal {
			writeJSON(w, 409, map[string]any{"error": "run active"})
			return
		}
	}
	script, ok := f.scripts[s.ID]
	if !ok {
		script = f.Default
	}
	f.next++
	id := fmt.Sprintf("run_%032x", f.next)
	run := &Run{ID: id, SessionID: s.ID, script: script, gate: make(chan struct{}), cut: map[int]bool{}, stall: map[int]bool{}}
	for _, i := range script.CutAfter {
		run.cut[i] = true
	}
	for _, i := range script.StallAfter {
		run.stall[i] = true
	}
	for _, fr := range script.Fixture.Frames {
		c := map[string]any{}
		for k, v := range fr {
			c[k] = v
		}
		c["run_id"] = id
		b, _ := json.Marshal(c)
		run.frames = append(run.frames, b)
	}
	f.runs[id] = run
	f.RunInputs = append(f.RunInputs, body.Input)
	if key != "" {
		f.idem[key] = id
	}
	writeJSON(w, 202, map[string]any{"run_id": id, "status": "started", "replayed": false})
}

func (f *Fake) runFor(w http.ResponseWriter, r *http.Request) *Run {
	run := f.runs[r.PathValue("id")]
	if run == nil {
		writeJSON(w, 404, map[string]any{"error": "run not found"})
	}
	return run
}

func rewrite(body map[string]any, run *Run) map[string]any {
	out := map[string]any{}
	for k, v := range body {
		out[k] = v
	}
	out["run_id"], out["session_id"] = run.ID, run.SessionID
	if a, ok := out["approval"].(map[string]any); ok {
		c := map[string]any{}
		for k, v := range a {
			c[k] = v
		}
		c["run_id"] = run.ID
		out["approval"] = c
	}
	return out
}

func (f *Fake) getRun(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run := f.runFor(w, r)
	if run == nil {
		return
	}
	fx := run.script.Fixture
	switch {
	case run.StatusOverride != "":
		body := rewrite(fx.Final, run)
		body["status"] = run.StatusOverride
		writeJSON(w, 200, body)
	case run.terminal:
		writeJSON(w, 200, rewrite(fx.Final, run))
	case fx.GateOn == "approval" && !run.gateOpen && run.sent >= fx.GateAt && fx.Pending != nil:
		writeJSON(w, 200, rewrite(fx.Pending, run))
	default:
		writeJSON(w, 200, map[string]any{"object": "hermes.run", "run_id": run.ID, "session_id": run.SessionID, "status": "running"})
	}
}

func (f *Fake) openGate(run *Run) {
	if !run.gateOpen {
		run.gateOpen = true
		close(run.gate)
	}
}

func (f *Fake) approve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	run := f.runFor(w, r)
	if run == nil {
		return
	}
	call := ApprovalCall{RunID: run.ID}
	call.RequestID, _ = body["request_id"].(string)
	call.Choice, _ = body["choice"].(string)
	_, call.HasGeneration = body["execution_generation"]
	f.Approvals = append(f.Approvals, call)
	if run.script.Fixture.GateOn != "approval" || run.gateOpen {
		writeJSON(w, 409, map[string]any{"error": "no pending approval"})
		return
	}
	f.openGate(run)
	writeJSON(w, 200, map[string]any{"object": "hermes.run.approval_response", "run_id": run.ID,
		"choice": call.Choice, "request_id": call.RequestID, "resolved": 1})
}

func (f *Fake) stop(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run := f.runFor(w, r)
	if run == nil {
		return
	}
	f.Stops = append(f.Stops, run.ID)
	if run.script.Fixture.GateOn == "stop" {
		f.openGate(run)
	}
	writeJSON(w, 200, map[string]any{"run_id": run.ID, "status": "stopping"})
}

func (f *Fake) steer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	run := f.runFor(w, r)
	if run == nil {
		return
	}
	if run.terminal {
		writeJSON(w, 409, map[string]any{"error": "run not running"})
		return
	}
	f.Steers = append(f.Steers, body.Text)
	writeJSON(w, 200, map[string]any{"run_id": run.ID, "status": "queued"})
}

func (f *Fake) events(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	run := f.runFor(w, r)
	if run == nil {
		f.mu.Unlock()
		return
	}
	if run.EventsGone {
		f.mu.Unlock()
		writeJSON(w, 404, map[string]any{"error": "gone"})
		return
	}
	run.Streams++
	start := run.sent
	if run.script.FromStart {
		start = 0
	}
	f.mu.Unlock()

	fl := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(": open\n\n"))
	fl.Flush()
	ka := time.NewTicker(f.KeepAlive)
	defer ka.Stop()
	fx := run.script.Fixture
	waitFor := func(ch <-chan struct{}, delay time.Duration) bool {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		for {
			select {
			case <-r.Context().Done():
				return false
			case <-ka.C:
				if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
					return false
				}
				fl.Flush()
			case <-ch:
				ch = nil
				if delay == 0 {
					return true
				}
			case <-timer.C:
				delay = 0
				if ch == nil {
					return true
				}
			}
		}
	}
	closed := make(chan struct{})
	close(closed)
	for i := start; i < len(run.frames); i++ {
		var delay time.Duration
		if f.Speed > 0 && i > 0 {
			delay = time.Duration((fx.Times[i] - fx.Times[i-1]) * f.Speed * float64(time.Second))
		}
		gate := (<-chan struct{})(closed)
		if fx.GateAt == i {
			gate = run.gate
		}
		if !waitFor(gate, delay) {
			return
		}
		if h, ok := run.script.Hold[i]; ok && !waitFor(h, 0) {
			return
		}
		var buf bytes.Buffer
		buf.WriteString("data: ")
		buf.Write(run.frames[i])
		buf.WriteString("\n\n")
		if _, err := w.Write(buf.Bytes()); err != nil {
			return
		}
		fl.Flush()
		f.mu.Lock()
		run.sent = max(run.sent, i+1)
		if i == len(run.frames)-1 {
			run.terminal = true
		}
		cut, stall := run.cut[i], run.stall[i]
		delete(run.cut, i)
		delete(run.stall, i)
		f.mu.Unlock()
		if cut {
			return
		}
		if stall {
			<-r.Context().Done()
			return
		}
	}
}
