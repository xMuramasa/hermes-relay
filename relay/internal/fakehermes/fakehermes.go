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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
	// StartedAt is a unix time in seconds, like Hermes' started_at.
	StartedAt any `json:"started_at,omitempty"`
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
	RunInputs   []string // the text, or the raw JSON of a list input
	messages    map[string][]map[string]any
	RunModels   []string // "provider/model" per created run, "" when the body named none
	Approvals   []ApprovalCall
	Stops       []string
	Steers      []string
	Paths       []string // every request, "METHOD /path"
	SessionPost []map[string]any
	// ModelOptions is GET /api/model/options with ?include_unconfigured=true; false leaves out providers
	// with "authenticated": false.
	ModelOptions map[string]any
	optionsFail  int
	modelLocks   map[string][2]string // accepted POST /api/sessions/{id}/model, {provider, model}
	jobs         map[string]map[string]any
	jobOrder     []string
	jobsFail     int
	jobBodies    []map[string]any // bodies of POST and PATCH /api/jobs[/{id}], as received
}

// DefaultModelOptions is shaped like Hermes' answer (2026-10-31), trimmed to a few models.
func DefaultModelOptions() map[string]any {
	reasoning := map[string]any{"fast": false, "reasoning": true}
	return map[string]any{
		"model": "claude-opus-5-5[1m]", "provider": "claude-subscription-directsdk-experimental",
		"providers": []any{
			map[string]any{"slug": "claude-subscription-directsdk-experimental", "name": "Claude subscription",
				"is_current": true, "is_user_defined": false, "models": []any{"claude-opus-5-5[1m]", "claude-sonnet-5-5"},
				"total_models": 2, "source": "built-in", "authenticated": true, "featured_models": []any{},
				"capabilities": map[string]any{"claude-opus-5-5[1m]": reasoning, "claude-sonnet-5-5": reasoning}},
			map[string]any{"slug": "anthropic", "name": "Anthropic", "is_current": false, "models": []any{"claude-haiku-5-5"},
				"total_models": 1, "source": "built-in", "authenticated": true,
				"capabilities": map[string]any{"claude-haiku-5-5": map[string]any{"fast": true, "reasoning": false}}},
			map[string]any{"slug": "copilot", "name": "GitHub Copilot", "is_current": false, "models": []any{"gpt-5.5"},
				"total_models": 1, "source": "built-in", "authenticated": true, "capabilities": map[string]any{}},
			map[string]any{"slug": "openrouter", "name": "OpenRouter", "models": []any{"some/model"}, "authenticated": false},
		},
	}
}

// New starts a fake Hermes. Close it with t.Cleanup(f.Close).
func New(key string, def *Fixture) *Fake {
	f := &Fake{
		Key: key, KeepAlive: 50 * time.Millisecond,
		sessions: map[string]*Session{}, runs: map[string]*Run{}, idem: map[string]string{}, scripts: map[string]Script{},
		Default:      Script{Fixture: def, FromStart: true},
		ModelOptions: DefaultModelOptions(), modelLocks: map[string][2]string{}, jobs: map[string]map[string]any{},
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
	mux.HandleFunc("GET /api/sessions/{id}/messages", f.sessionMessages)
	mux.HandleFunc("POST /api/sessions/{id}/model", f.lockModel)
	mux.HandleFunc("GET /api/model/options", f.modelOptions)
	mux.HandleFunc("POST /v1/runs", f.createRun)
	mux.HandleFunc("GET /v1/runs/{id}", f.getRun)
	mux.HandleFunc("GET /v1/runs/{id}/events", f.events)
	mux.HandleFunc("POST /v1/runs/{id}/approval", f.approve)
	mux.HandleFunc("POST /v1/runs/{id}/stop", f.stop)
	mux.HandleFunc("POST /v1/runs/{id}/steer", f.steer)
	mux.HandleFunc("GET /api/jobs", f.listJobs)
	mux.HandleFunc("POST /api/jobs", f.createJob)
	mux.HandleFunc("GET /api/jobs/{id}", f.jobRoute)
	mux.HandleFunc("PATCH /api/jobs/{id}", f.jobRoute)
	mux.HandleFunc("DELETE /api/jobs/{id}", f.jobRoute)
	mux.HandleFunc("POST /api/jobs/{id}/{action}", f.jobRoute)
	mux.ServeHTTP(w, r)
}

// listSessions honors source, and limit/offset when given (Hermes defaults limit to 50; the fake to everything).
func (f *Fake) listSessions(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	data := []*Session{}
	for _, id := range f.order {
		if s := f.sessions[id]; s != nil && (q.Get("source") == "" || s.Source == q.Get("source")) {
			data = append(data, s)
		}
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	data = data[min(offset, len(data)):]
	more := false
	if limit, err := strconv.Atoi(q.Get("limit")); err == nil && limit < len(data) {
		data, more = data[:limit], true
	}
	writeJSON(w, 200, map[string]any{"data": data, "has_more": more})
}

// AddCronRun adds the session of one run of a cron job, as Hermes names it (stamp: YYYYMMDD_HHMMSS).
func (f *Fake) AddCronRun(jobID, stamp string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "cron_" + jobID + "_" + stamp
	f.sessions[id] = &Session{ID: id, Source: "cron", Title: "run " + stamp, StartedAt: 1792800000.5}
	f.order = append(f.order, id)
	return id
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

// SetMessages sets a session's history (Hermes' message objects, oldest first).
func (f *Fake) SetMessages(sessionID string, msgs []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.messages == nil {
		f.messages = map[string][]map[string]any{}
	}
	f.messages[sessionID] = msgs
}

// RunInputsSeen returns the input of each created run, in order.
func (f *Fake) RunInputsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.RunInputs...)
}

// sessionMessages honors limit and offset (default limit 500, like Hermes).
func (f *Fake) sessionMessages(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.session(w, r)
	if s == nil {
		return
	}
	data := f.messages[s.ID]
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil {
		limit = 500
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	data = data[min(offset, len(data)):]
	data = data[:min(limit, len(data))]
	writeJSON(w, 200, map[string]any{"object": "list", "session_id": s.ID, "data": append([]map[string]any{}, data...),
		"pagination": map[string]any{"limit": limit, "offset": offset, "returned": len(data)}})
}

// FailModelOptions makes GET /api/model/options answer status (0 to stop).
func (f *Fake) FailModelOptions(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.optionsFail = status
}

func (f *Fake) modelOptions(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.optionsFail != 0 {
		writeJSON(w, f.optionsFail, map[string]any{"error": map[string]any{"code": "model_options_failed"}})
		return
	}
	out := map[string]any{}
	for k, v := range f.ModelOptions {
		out[k] = v
	}
	if r.URL.Query().Get("include_unconfigured") == "false" {
		var ps []any
		for _, p := range f.ModelOptions["providers"].([]any) {
			if p.(map[string]any)["authenticated"] != false {
				ps = append(ps, p)
			}
		}
		out["providers"] = ps
	}
	writeJSON(w, 200, out)
}

// ModelLock returns a session's accepted model lock, {provider, model}.
// RunModelsSeen returns the provider/model each created run named, in order ("" for none).
func (f *Fake) RunModelsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.RunModels...)
}

func (f *Fake) ModelLock(id string) [2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modelLocks[id]
}

var prefixRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{2,64}$`)

// cleanID is Hermes' _clean_runtime_id: trimmed, "" if too long or holding CR, LF or NUL.
func cleanID(v any, maxLen int) string {
	s := ""
	if v != nil {
		s = strings.TrimSpace(fmt.Sprint(v))
	}
	if len(s) > maxLen || strings.ContainsAny(s, "\r\n\x00") {
		return ""
	}
	return s
}

// lockModel is Hermes' _handle_session_model_lock without model_routes aliases: {model|model_id,
// provider|provider_id}, "provider::model" also accepted; the virtual model "hermes-agent" counts as none.
func (f *Fake) lockModel(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.session(w, r)
	if s == nil {
		return
	}
	var body map[string]any
	if json.NewDecoder(r.Body).Decode(&body) != nil || body == nil {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "Invalid JSON in request body"}})
		return
	}
	or := func(a, b string) string {
		if body[a] != nil && body[a] != "" {
			return a
		}
		return b
	}
	model := cleanID(body[or("model", "model_id")], 200)
	provider := cleanID(body[or("provider", "provider_id")], 80)
	if p, m, ok := strings.Cut(model, "::"); ok && prefixRe.MatchString(p) && strings.TrimSpace(m) != "" {
		model = strings.TrimSpace(m)
		if provider == "" {
			provider = p
		}
	}
	if model == "hermes-agent" {
		model = ""
	}
	switch {
	case model == "" && provider == "":
		writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "missing_model"}})
	case model == "":
		writeJSON(w, 409, map[string]any{"error": map[string]any{"code": "model_lock_unavailable"}})
	default:
		f.modelLocks[s.ID] = [2]string{provider, model}
		writeJSON(w, 200, map[string]any{"object": "hermes.session.model_lock", "session_id": s.ID,
			"runtime": map[string]any{"provider": provider, "model": model, "route_source": "raw_request",
				"requested": map[string]any{"provider": provider, "model": model}, "model_lock": "accepted"}})
	}
}

func (f *Fake) createRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Input     json.RawMessage `json:"input"`
		SessionID string          `json:"session_id"`
		Provider  string          `json:"provider"`
		Model     string          `json:"model"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad json"})
		return
	}
	// input: a string, or a list of {role, content} messages (content: a string or text/image_url parts).
	input := ""
	if json.Unmarshal(body.Input, &input) != nil {
		var msgs []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(body.Input, &msgs) != nil || len(msgs) == 0 || msgs[len(msgs)-1].Role != "user" {
			writeJSON(w, 400, map[string]any{"error": "input must be a string or a list of messages ending with a user one"})
			return
		}
		input = string(body.Input)
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
	f.RunInputs = append(f.RunInputs, input)
	if body.Provider != "" || body.Model != "" {
		f.RunModels = append(f.RunModels, body.Provider+"/"+body.Model)
	} else {
		f.RunModels = append(f.RunModels, "")
	}
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

// Cron jobs: Hermes' /api/jobs (gateway/platforms/api_server.py, 2026-11-01) with its validation, trimmed.

var jobIDRe = regexp.MustCompile(`^[a-f0-9]{12}$`)

// jobUpdateFields is Hermes' _UPDATE_ALLOWED_FIELDS; other keys in a PATCH are dropped.
var jobUpdateFields = map[string]bool{"name": true, "schedule": true, "prompt": true, "deliver": true, "skills": true,
	"skill": true, "repeat": true, "enabled": true}

// FailJobs makes every /api/jobs call answer status (0 to stop); 501 is a Hermes without cron.
func (f *Fake) FailJobs(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobsFail = status
}

// JobBodies returns the bodies of POST and PATCH /api/jobs[/{id}] as received.
func (f *Fake) JobBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.jobBodies...)
}

// Job returns a copy of a stored job, or nil.
func (f *Fake) Job(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobs[id] == nil {
		return nil
	}
	c := map[string]any{}
	for k, v := range f.jobs[id] {
		c[k] = v
	}
	return c
}

// AddJob stores a job shaped like cron/jobs.py's record, with fields overriding the defaults; a string schedule is
// parsed as a cron expression. It returns the id.
func (f *Fake) AddJob(fields map[string]any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addJobLocked(fields)
}

func (f *Fake) addJobLocked(fields map[string]any) string {
	f.next++
	id := fmt.Sprintf("%012x", f.next)
	job := map[string]any{"id": id, "name": "job " + id, "prompt": "", "skills": []any{}, "skill": nil, "model": nil,
		"provider": nil, "base_url": nil, "script": nil, "no_agent": false, "context_from": nil, "schedule": "0 9 * * *",
		"repeat": map[string]any{"times": nil, "completed": 0}, "enabled": true, "state": "scheduled", "paused_at": nil,
		"paused_reason": nil, "created_at": "2026-11-01T09:00:00+00:00", "next_run_at": "2026-11-02T09:00:00+00:00",
		"last_run_at": nil, "last_status": nil, "last_error": nil, "last_delivery_error": nil, "failure_streak": 0,
		"deliver": "local", "origin": map[string]any{"platform": "api_server", "chat_id": "secret-chat"},
		"enabled_toolsets": nil, "workdir": nil}
	for k, v := range fields {
		job[k] = v
	}
	setSchedule(job)
	f.jobs[id] = job
	f.jobOrder = append(f.jobOrder, id)
	return id
}

func setSchedule(job map[string]any) {
	if s, ok := job["schedule"].(string); ok {
		job["schedule"] = map[string]any{"kind": "cron", "expr": s, "display": s}
		job["schedule_display"] = s
	}
}

func jobErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// jobsDown answers the configured failure, if any.
func (f *Fake) jobsDown(w http.ResponseWriter) bool {
	if f.jobsFail != 0 {
		jobErr(w, f.jobsFail, "Cron module not available")
	}
	return f.jobsFail != 0
}

func (f *Fake) listJobs(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobsDown(w) {
		return
	}
	all := r.URL.Query().Get("include_disabled") == "true"
	jobs := []map[string]any{}
	for _, id := range f.jobOrder {
		if j := f.jobs[id]; j != nil && (all || j["enabled"] == true) {
			c := map[string]any{"latest_execution": nil}
			for k, v := range j {
				c[k] = v
			}
			jobs = append(jobs, c)
		}
	}
	writeJSON(w, 200, map[string]any{"jobs": jobs})
}

// checkJobText is Hermes' name and prompt caps.
func checkJobText(w http.ResponseWriter, body map[string]any) bool {
	if n, ok := body["name"].(string); ok && utf8.RuneCountInString(n) > 200 {
		jobErr(w, 400, "Name must be ≤ 200 characters")
		return false
	}
	if p, ok := body["prompt"].(string); ok && utf8.RuneCountInString(p) > 5000 {
		jobErr(w, 400, "Prompt must be ≤ 5000 characters")
		return false
	}
	return true
}

func (f *Fake) createJob(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if json.NewDecoder(r.Body).Decode(&body) != nil || body == nil {
		jobErr(w, 500, "invalid JSON")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobsDown(w) {
		return
	}
	f.jobBodies = append(f.jobBodies, body)
	name, _ := body["name"].(string)
	schedule, _ := body["schedule"].(string)
	prompt, _ := body["prompt"].(string)
	paused, hasPaused := body["paused"]
	repeat, hasRepeat := body["repeat"].(float64)
	switch {
	case strings.TrimSpace(name) == "":
		jobErr(w, 400, "Name is required")
	case strings.TrimSpace(schedule) == "":
		jobErr(w, 400, "Schedule is required")
	case !checkJobText(w, body):
	case body["repeat"] != nil && (!hasRepeat || repeat != float64(int(repeat)) || repeat < 1):
		jobErr(w, 400, "Repeat must be a positive integer")
	case hasPaused && paused != true && paused != false:
		jobErr(w, 400, "paused must be a boolean.")
	case body["paused_reason"] != nil && paused != true:
		jobErr(w, 400, "paused_reason requires paused=True.")
	case strings.TrimSpace(prompt) == "" && body["skills"] == nil:
		jobErr(w, 400, "Cron job has nothing to run: the prompt is blank and no script or skill(s) are set. "+
			"Provide a prompt, a script, or at least one skill.")
	default:
		fields := map[string]any{"name": strings.TrimSpace(name), "schedule": strings.TrimSpace(schedule),
			"prompt": strings.TrimSpace(prompt), "enabled": paused != true}
		for _, k := range []string{"deliver", "skills", "paused_reason"} {
			if v, ok := body[k]; ok {
				fields[k] = v
			}
		}
		if paused == true {
			fields["state"], fields["paused_at"], fields["next_run_at"] = "paused", "2026-11-01T10:00:00+00:00", nil
		}
		if hasRepeat {
			fields["repeat"] = map[string]any{"times": repeat, "completed": 0}
		}
		writeJSON(w, 200, map[string]any{"job": f.jobs[f.addJobLocked(fields)]})
	}
}

// jobRoute is GET/PATCH/DELETE /api/jobs/{id} and POST /api/jobs/{id}/pause|resume|run.
func (f *Fake) jobRoute(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if r.Method == http.MethodPatch && (json.NewDecoder(r.Body).Decode(&body) != nil || body == nil) {
		jobErr(w, 500, "invalid JSON")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobsDown(w) {
		return
	}
	id := r.PathValue("id")
	if !jobIDRe.MatchString(id) {
		jobErr(w, 400, "Invalid job ID format")
		return
	}
	if body != nil {
		f.jobBodies = append(f.jobBodies, body)
	}
	job := f.jobs[id]
	if job == nil {
		jobErr(w, 404, "Job not found")
		return
	}
	switch r.Method + " " + r.PathValue("action") {
	case "GET ":
	case "DELETE ":
		delete(f.jobs, id)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	case "PATCH ":
		updates := map[string]any{}
		for k, v := range body {
			if jobUpdateFields[k] {
				updates[k] = v
			}
		}
		if len(updates) == 0 {
			jobErr(w, 400, "No valid fields to update")
			return
		}
		if !checkJobText(w, updates) {
			return
		}
		if s, ok := updates["schedule"].(string); ok && strings.TrimSpace(s) == "never" {
			// Hermes' update raises inside _job_response, which answers 500 with the message.
			jobErr(w, 500, "Invalid schedule 'never'")
			return
		}
		for k, v := range updates {
			if k == "repeat" {
				v = map[string]any{"times": v, "completed": 0}
			}
			job[k] = v
		}
		setSchedule(job)
	case "POST pause":
		job["enabled"], job["state"], job["paused_at"] = false, "paused", "2026-11-01T10:00:00+00:00"
	case "POST resume":
		job["enabled"], job["state"], job["paused_at"], job["paused_reason"] = true, "scheduled", nil, nil
	case "POST run":
		job["enabled"], job["state"], job["next_run_at"] = true, "scheduled", "2026-11-01T10:00:00+00:00"
	default:
		jobErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, map[string]any{"job": job})
}
