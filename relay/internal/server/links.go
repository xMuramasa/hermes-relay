package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

const maxChainLinks = 32

// listLinks answers every handoff link. Like tags, ids of sessions deleted elsewhere may linger.
func (s *Server) listLinks(w http.ResponseWriter, r *http.Request, _ store.Device) {
	links, err := s.store.SessionLinks(r.Context())
	if err != nil {
		s.log.Error("reading links failed", "err", err.Error())
		writeError(w, protocol.Errorf(protocol.CodeInternal, "internal error"), 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"continues": links})
}

// setContinues records that a conversation continues an older one ({"from": id}), or drops that ({"from": null}).
func (s *Server) setContinues(w http.ResponseWriter, r *http.Request, dev store.Device) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	var body struct {
		From json.RawMessage `json:"from"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, err, 0)
		return
	}
	var from *string
	if len(body.From) == 0 || json.Unmarshal(body.From, &from) != nil || from != nil && !protocol.ValidID(*from) {
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "from must be a session id, or null to remove the link"), 0)
		return
	}
	if from == nil {
		if err := s.store.SetContinues(r.Context(), id, "", maxChainLinks); err != nil {
			s.log.Error("removing link failed", "session_id", id, "err", err.Error())
			writeError(w, protocol.Errorf(protocol.CodeInternal, "internal error"), 0)
			return
		}
		s.log.Info("session link removed", "session_id", id, "device", dev.Name)
		writeJSON(w, http.StatusOK, map[string]any{"from": nil})
		return
	}
	if *from == id {
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "a conversation can't continue itself"), 0)
		return
	}
	for _, sid := range []string{id, *from} {
		if _, ok := s.sessionSource(w, r, sid); !ok {
			return
		}
	}
	err := s.store.SetContinues(r.Context(), id, *from, maxChainLinks)
	switch {
	case errors.Is(err, store.ErrLinkTaken), errors.Is(err, store.ErrLinkCycle), errors.Is(err, store.ErrChainTooLong):
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "%s", err.Error()), 0)
		return
	case err != nil:
		s.log.Error("setting link failed", "session_id", id, "err", err.Error())
		writeError(w, protocol.Errorf(protocol.CodeInternal, "internal error"), 0)
		return
	}
	s.log.Info("session link set", "session_id", id, "continues", *from, "device", dev.Name)
	writeJSON(w, http.StatusOK, map[string]any{"from": *from})
}
