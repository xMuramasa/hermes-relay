package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"

	"github.com/xMuramasa/trismegistus/relay/internal/agents"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// Background agents: the host helper trismegistus-agents (agents/) lists Hermes' background processes and Claude
// Code delegate runs, follows their transcripts and stops them. The relay checks ids and passes the helper's JSON on.

const codeNotConfigured = "not_configured" // REST-only, like GitHub's

var (
	agentIDRe = regexp.MustCompile(`^(proc_[A-Za-z0-9]{1,64}|run_[A-Za-z0-9][A-Za-z0-9._-]{0,63})$`)
	afterRe   = regexp.MustCompile(`^[0-9]{1,9}$`)
)

// SetAgents turns on the /api/agents endpoints; without it they answer not_configured. Call it before serving.
func (s *Server) SetAgents(c *agents.Client) { s.agents = c }

// forwardAgents asks the helper and writes its answer; it returns the outcome for logs ("ok" or an error code).
func (s *Server) forwardAgents(w http.ResponseWriter, r *http.Request, method, path string, q url.Values) string {
	notConfigured := func(msg string) string {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"code": codeNotConfigured, "message": msg}})
		return codeNotConfigured
	}
	if s.agents == nil {
		return notConfigured("background agents are not set up on the relay (AGENTS_SOCKET)")
	}
	st, b, err := s.agents.Do(r.Context(), method, path, q)
	if err != nil {
		s.log.Warn("agents helper unreachable", "err", err.Error())
		return notConfigured("the agents helper is not running on the host")
	}
	var out struct {
		Error *struct{ Code string } `json:"error"`
	}
	if json.Unmarshal(b, &out) != nil {
		writeError(w, protocol.Errorf(protocol.CodeInternal, "the agents helper sent an invalid response"), 0)
		return protocol.CodeInternal
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(st)
	_, _ = w.Write(b)
	if out.Error != nil {
		return out.Error.Code
	}
	return "ok"
}

func agentPath(w http.ResponseWriter, r *http.Request, action string) (string, bool) {
	id := r.PathValue("id")
	if !agentIDRe.MatchString(id) {
		writeError(w, protocol.Errorf(protocol.CodeNotFound, "not found"), 0)
		return "", false
	}
	return "/agents/" + id + "/" + action, true
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request, _ store.Device) {
	s.forwardAgents(w, r, http.MethodGet, "/agents", nil)
}

func (s *Server) agentTranscript(w http.ResponseWriter, r *http.Request, _ store.Device) {
	path, ok := agentPath(w, r, "transcript")
	if !ok {
		return
	}
	after := r.URL.Query().Get("after")
	if after == "" {
		after = "0"
	}
	if !afterRe.MatchString(after) {
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "after must be a line number"), 0)
		return
	}
	s.forwardAgents(w, r, http.MethodGet, path, url.Values{"after": {after}})
}

func (s *Server) stopAgent(w http.ResponseWriter, r *http.Request, dev store.Device) {
	if path, ok := agentPath(w, r, "stop"); ok {
		outcome := s.forwardAgents(w, r, http.MethodPost, path, nil)
		s.log.Info("agent stop", "agent_id", r.PathValue("id"), "device", dev.Name, "outcome", outcome)
	}
}
