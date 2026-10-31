package server

import (
	"net/http"

	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// codeForbidden is REST-only: the WebSocket protocol has no roles, so it is not in protocol/schema.json.
const codeForbidden = "forbidden"

// requireRole lets only devices with the given role through (use it inside authed).
func requireRole(role string, next func(http.ResponseWriter, *http.Request, store.Device)) func(http.ResponseWriter, *http.Request, store.Device) {
	return func(w http.ResponseWriter, r *http.Request, dev store.Device) {
		if dev.Role != role {
			writeError(w, protocol.Errorf(codeForbidden, "this device is a %s; this needs a %s", dev.Role, role), 0)
			return
		}
		next(w, r, dev)
	}
}

// handleMe tells the app which device it is and what it may do.
func (s *Server) handleMe(w http.ResponseWriter, _ *http.Request, dev store.Device) {
	writeJSON(w, http.StatusOK, map[string]string{"device": dev.Name, "role": dev.Role})
}
