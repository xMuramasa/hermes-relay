// Package server is the relay's HTTP side: the REST allowlist in front of Hermes, pairing, health, and the
// WebSocket endpoint for the app.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/xMuramasa/trismegistus/relay/internal/agents"
	"github.com/xMuramasa/trismegistus/relay/internal/github"
	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/hub"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// Options are the server's limits and timings.
type Options struct {
	Version             string
	TrustedProxies      []netip.Prefix
	AuthTimeout         time.Duration // first WebSocket message must be auth within this
	PingInterval        time.Duration // WebSocket ping period; two missed pongs drop the socket
	RevokeCheck         time.Duration // how often sockets of revoked devices are looked for
	CleanupInterval     time.Duration // how often old events and codes are deleted
	MaxDevices          int
	MaxSocketsPerDevice int
	PromptsPerMinute    int
	PairPerMinute       int // per client IP
	PairPerMinuteGlobal int
	SendQueue           int // queued outbound messages per socket before it's dropped as too slow
	MaxSubscriptions    int // sessions per socket
	MaxInFlight         int // concurrent prompt/stop/steer/approve per socket
}

// DefaultOptions are SPEC.md's limits.
func DefaultOptions() Options {
	return Options{
		Version:             "dev",
		AuthTimeout:         5 * time.Second,
		PingInterval:        25 * time.Second,
		RevokeCheck:         2 * time.Second,
		CleanupInterval:     time.Hour,
		MaxDevices:          5,
		MaxSocketsPerDevice: 3,
		PromptsPerMinute:    20,
		PairPerMinute:       10,
		PairPerMinuteGlobal: 30,
		SendQueue:           8192,
		MaxSubscriptions:    64,
		MaxInFlight:         4,
	}
}

// Server serves HTTP and WebSocket requests.
type Server struct {
	opts      Options
	store     *store.Store
	hub       *hub.Hub
	hermes    *hermes.Client
	github    *github.Client // nil: the /api/github endpoints answer not_configured
	agents    *agents.Client // nil: the /api/agents endpoints answer not_configured
	log       *slog.Logger
	validator *protocol.Validator

	pairIP, pairAll, prompts *limiter
	modelCache               modelCache
	media                    mediaCache

	termSocket string // empty: /api/terminal answers not_configured

	mu      sync.Mutex
	closing bool
	conns   map[int64][]*wsConn // by device id, oldest first
	terms   map[*wsConn]bool    // open /api/terminal sockets
}

// New makes a server.
func New(opts Options, st *store.Store, h *hub.Hub, hc *hermes.Client, log *slog.Logger) (*Server, error) {
	v, err := protocol.NewValidator()
	if err != nil {
		return nil, err
	}
	return &Server{
		opts: opts, store: st, hub: h, hermes: hc, log: log, validator: v,
		pairIP:  newLimiter(opts.PairPerMinute, time.Minute),
		pairAll: newLimiter(opts.PairPerMinuteGlobal, time.Minute),
		prompts: newLimiter(opts.PromptsPerMinute, time.Minute),
		conns:   map[int64][]*wsConn{},
		terms:   map[*wsConn]bool{},
	}, nil
}

// Handler returns the routes. Anything not listed is 404: no other Hermes route is reachable.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /api/pair", s.logged(s.handlePair))
	mux.HandleFunc("GET /api/me", s.logged(s.authed(s.handleMe)))
	mux.HandleFunc("GET /api/sessions", s.logged(s.authed(s.listSessions)))
	mux.HandleFunc("POST /api/sessions", s.logged(s.authed(s.createSession)))
	mux.HandleFunc("GET /api/sessions/{id}", s.logged(s.authed(s.getSession)))
	mux.HandleFunc("PATCH /api/sessions/{id}", s.logged(s.authed(s.updateSession)))
	mux.HandleFunc("DELETE /api/sessions/{id}", s.logged(s.authed(s.deleteSession)))
	mux.HandleFunc("GET /api/sessions/{id}/messages", s.logged(s.authed(s.sessionMessages)))
	mux.HandleFunc("PUT /api/sessions/{id}/tags", s.logged(s.authed(s.setTags)))
	mux.HandleFunc("GET /api/tags", s.logged(s.authed(s.listTags)))
	mux.HandleFunc("GET /api/models", s.logged(s.authed(s.listModels)))
	mux.HandleFunc("PUT /api/sessions/{id}/model", s.logged(s.authed(s.setSessionModel)))
	mux.HandleFunc("GET /api/session-models", s.logged(s.authed(s.listSessionModels)))
	mux.HandleFunc("PUT /api/sessions/{id}/continues", s.logged(s.authed(s.setContinues)))
	mux.HandleFunc("GET /api/links", s.logged(s.authed(s.listLinks)))
	// Cron jobs (jobs.go): every device reads, managers write.
	manage := func(h func(http.ResponseWriter, *http.Request, store.Device)) http.HandlerFunc {
		return s.logged(s.authed(requireRole(store.RoleManager, h)))
	}
	mux.HandleFunc("GET /api/jobs", s.logged(s.authed(s.listJobs)))
	mux.HandleFunc("GET /api/jobs/{id}", s.logged(s.authed(s.getJob)))
	mux.HandleFunc("GET /api/jobs/{id}/runs", s.logged(s.authed(s.jobRuns)))
	mux.HandleFunc("POST /api/jobs", manage(s.createJob))
	mux.HandleFunc("PATCH /api/jobs/{id}", manage(s.updateJob))
	mux.HandleFunc("DELETE /api/jobs/{id}", manage(s.deleteJob))
	mux.HandleFunc("POST /api/jobs/{id}/pause", manage(s.jobAction("pause")))
	mux.HandleFunc("POST /api/jobs/{id}/resume", manage(s.jobAction("resume")))
	mux.HandleFunc("POST /api/jobs/{id}/run", manage(s.jobAction("run")))
	// GitHub (#21): reads for every device, writes for managers.
	mux.HandleFunc("GET /api/github/board", s.logged(s.authed(s.gh(s.githubBoard))))
	mux.HandleFunc("GET /api/github/repos", s.logged(s.authed(s.gh(s.githubRepos))))
	mux.HandleFunc("GET /api/github/repos/{owner}/{repo}/issues", s.logged(s.authed(s.gh(s.githubIssues))))
	mux.HandleFunc("GET /api/github/repos/{owner}/{repo}/issues/{number}", s.logged(s.authed(s.gh(s.githubIssue))))
	mux.HandleFunc("POST /api/github/repos/{owner}/{repo}/issues/{number}/comments", s.ghWrite(s.githubComment))
	mux.HandleFunc("PATCH /api/github/repos/{owner}/{repo}/issues/{number}", s.ghWrite(s.githubSetState))
	mux.HandleFunc("PUT /api/github/repos/{owner}/{repo}/issues/{number}/board", s.ghWrite(s.githubSetBoard))
	// Background agents (agents.go, #22): every device reads, managers stop.
	mux.HandleFunc("GET /api/agents", s.logged(s.authed(s.listAgents)))
	mux.HandleFunc("GET /api/agents/{id}/transcript", s.logged(s.authed(s.agentTranscript)))
	mux.HandleFunc("POST /api/agents/{id}/stop", manage(s.stopAgent))
	// Image attachments (attachments.go, #25): managers upload, every device downloads.
	mux.HandleFunc("POST /api/attachments", manage(s.uploadAttachment))
	mux.HandleFunc("GET /api/attachments/{id}", s.logged(s.authed(s.getAttachment)))
	// Images on the host named in a session's messages (media.go, #26), through the agents helper.
	mux.HandleFunc("GET /api/sessions/{id}/media", s.logged(s.authed(s.sessionMedia)))
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /api/terminal", s.handleTerminal) // terminal.go, #31
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, protocol.Errorf(protocol.CodeNotFound, "not found"), 0)
	})
	return mux
}

// Run does the background work (dropping revoked devices' sockets and terminals, hourly cleanup) until ctx is done.
func (s *Server) Run(ctx context.Context) {
	revoke := time.NewTicker(s.opts.RevokeCheck)
	defer revoke.Stop()
	cleanup := time.NewTicker(s.opts.CleanupInterval)
	defer cleanup.Stop()
	s.cleanup(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-revoke.C:
			s.dropRevoked(ctx)
			s.dropTerminals(ctx)
		case <-cleanup.C:
			s.cleanup(ctx)
		}
	}
}

func (s *Server) cleanup(ctx context.Context) {
	n, err := s.store.Cleanup(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("cleanup failed", "err", err.Error())
		}
		return
	}
	s.log.Info("cleanup", "events_deleted", n)
}

func (s *Server) dropRevoked(ctx context.Context) {
	ids, err := s.store.DeviceIDs(ctx)
	if err != nil {
		return
	}
	s.mu.Lock()
	var gone []*wsConn
	for id, cs := range s.conns {
		if !ids[id] {
			gone = append(gone, cs...)
		}
	}
	s.mu.Unlock()
	for _, c := range gone {
		s.log.Info("device revoked, closing socket", "device", c.dev.Name)
		c.shut(websocket.StatusPolicyViolation, "device revoked")
	}
}

// CloseSockets closes every WebSocket with "going away" (shutdown) and refuses new ones. It returns once each
// socket's in-flight requests are done too, so a run started during shutdown is stored before the store closes.
func (s *Server) CloseSockets() {
	s.mu.Lock()
	s.closing = true
	var all []*wsConn
	for _, cs := range s.conns {
		all = append(all, cs...)
	}
	for w := range s.terms {
		all = append(all, w)
	}
	s.mu.Unlock()
	for _, c := range all {
		c.shut(websocket.StatusGoingAway, "relay restarting")
	}
	for _, c := range all {
		<-c.finished
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store")
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

var statusOf = map[string]int{
	protocol.CodeUnauthorized:      http.StatusUnauthorized,
	protocol.CodeBadRequest:        http.StatusBadRequest,
	protocol.CodeTooLarge:          http.StatusRequestEntityTooLarge,
	protocol.CodeRateLimited:       http.StatusTooManyRequests,
	protocol.CodeBusy:              http.StatusConflict,
	protocol.CodeNotFound:          http.StatusNotFound,
	protocol.CodeReadOnly:          http.StatusForbidden,
	codeForbidden:                  http.StatusForbidden,
	protocol.CodeReplayGap:         http.StatusGone,
	protocol.CodeHermesUnavailable: http.StatusServiceUnavailable,
	protocol.CodeHermesError:       http.StatusBadGateway,
	protocol.CodeInternal:          http.StatusInternalServerError,
}

// writeError writes {"error": {code, message}} with the matching HTTP status.
func writeError(w http.ResponseWriter, e *protocol.Error, retryAfter time.Duration) {
	st, ok := statusOf[e.Code]
	if !ok {
		st = http.StatusInternalServerError
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	}
	writeJSON(w, st, map[string]any{"error": map[string]string{"code": e.Code, "message": e.Message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// logged logs the route pattern (never the query or body), status and duration.
func (s *Server) logged(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next(rec, r)
		s.log.Info("http", "method", r.Method, "route", r.Pattern, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	}
}
