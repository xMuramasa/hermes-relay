package server

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/hub"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

const modelsTTL = 5 * time.Minute

type modelInfo struct {
	ID        string `json:"id"`
	Reasoning bool   `json:"reasoning"`
}

type providerInfo struct {
	Slug   string      `json:"slug"`
	Name   string      `json:"name"`
	Models []modelInfo `json:"models"`
}

type modelList struct {
	Current   store.SessionModel `json:"current"`
	Providers []providerInfo     `json:"providers"`
}

// has reports whether the model is one of the provider's.
func (l *modelList) has(m store.SessionModel) bool {
	for _, p := range l.Providers {
		if p.Slug == m.Provider {
			return slices.ContainsFunc(p.Models, func(i modelInfo) bool { return i.ID == m.Model })
		}
	}
	return false
}

// modelCache keeps Hermes' model list for modelsTTL (successes only).
type modelCache struct {
	mu   sync.Mutex
	at   time.Time
	list *modelList
}

// models returns the connected providers' models, from the cache when fresh.
func (s *Server) models(ctx context.Context) (*modelList, error) {
	c := &s.modelCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.list != nil && time.Since(c.at) < modelsTTL {
		return c.list, nil
	}
	opts, err := s.hermes.GetModelOptions(ctx)
	if err != nil {
		return nil, err
	}
	list := &modelList{Current: store.SessionModel{Provider: opts.Provider, Model: opts.Model}, Providers: []providerInfo{}}
	for _, p := range opts.Providers {
		pi := providerInfo{Slug: p.Slug, Name: p.Name, Models: []modelInfo{}}
		for _, id := range p.Models {
			pi.Models = append(pi.Models, modelInfo{ID: id, Reasoning: p.Capabilities[id].Reasoning})
		}
		list.Providers = append(list.Providers, pi)
	}
	c.list, c.at = list, time.Now()
	return list, nil
}

func (s *Server) hermesFailed(w http.ResponseWriter, err error) {
	if st := hermes.StatusOf(err); st != 0 {
		s.log.Warn("hermes refused", "hermes_status", st)
	}
	writeError(w, hermes.ProtocolError(err), hermes.RetryAfterOf(err))
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request, _ store.Device) {
	list, err := s.models(r.Context())
	if err != nil {
		s.hermesFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// setSessionModel locks a relay chat to one of the connected models, in Hermes and in the relay's own record.
func (s *Server) setSessionModel(w http.ResponseWriter, r *http.Request, dev store.Device) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	var m store.SessionModel
	if err := decodeBody(w, r, &m); err != nil {
		writeError(w, err, 0)
		return
	}
	list, err := s.models(r.Context())
	if err != nil {
		s.hermesFailed(w, err)
		return
	}
	if !list.has(m) {
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "not a model of a connected provider; see GET /api/models"), 0)
		return
	}
	src, ok := s.sessionSource(w, r, id)
	if !ok {
		return
	}
	// The lock is Hermes-wide state: other surfaces' sessions (Telegram...) keep their own model.
	if src != hub.SourceAPIServer {
		writeError(w, protocol.Errorf(protocol.CodeReadOnly, "only relay chats can change model"), 0)
		return
	}
	if err := s.hermes.LockSessionModel(r.Context(), id, m.Provider, m.Model); err != nil {
		s.hermesFailed(w, err)
		return
	}
	if err := s.store.SetSessionModel(r.Context(), id, m); err != nil {
		s.log.Error("storing session model failed", "session_id", id, "err", err.Error())
		writeError(w, protocol.Errorf(protocol.CodeInternal, "internal error"), 0)
		return
	}
	s.log.Info("session model set", "session_id", id, "device", dev.Name, "provider", m.Provider, "model", m.Model)
	writeJSON(w, http.StatusOK, m)
}

// listSessionModels answers the chosen models; sessions left out use the list's current model.
func (s *Server) listSessionModels(w http.ResponseWriter, r *http.Request, _ store.Device) {
	models, err := s.store.SessionModels(r.Context())
	if err != nil {
		s.log.Error("reading session models failed", "err", err.Error())
		writeError(w, protocol.Errorf(protocol.CodeInternal, "internal error"), 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": models})
}
