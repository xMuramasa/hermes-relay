package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// Search: the agents helper searches Hermes' message index (agents/search.py); the relay checks the query and passes
// the helper's JSON on. The query is never logged.

// searchCall is a device's search in flight; a newer one cancels it (the app searches as you type).
type searchCall struct{ cancel context.CancelFunc }

func (s *Server) search(w http.ResponseWriter, r *http.Request, dev store.Device) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if n := utf8.RuneCountInString(q); n < 2 || n > 200 {
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "q must be 2 to 200 characters"), 0)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		var err error
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 100 {
			writeError(w, protocol.Errorf(protocol.CodeBadRequest, "limit must be 1 to 100"), 0)
			return
		}
	}
	if s.agents == nil {
		writeError(w, protocol.Errorf(protocol.CodeHermesUnavailable, "search needs the agents helper"), 0)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	call := &searchCall{cancel}
	s.mu.Lock()
	if old := s.searches[dev.ID]; old != nil {
		old.cancel()
	}
	s.searches[dev.ID] = call
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.searches[dev.ID] == call {
			delete(s.searches, dev.ID)
		}
		s.mu.Unlock()
		cancel()
	}()

	st, b, err := s.agents.Search(ctx, q, limit)
	switch {
	case err != nil && ctx.Err() != nil && r.Context().Err() == nil:
		writeError(w, protocol.Errorf(protocol.CodeBusy, "a newer search replaced this one"), 0)
	case err != nil:
		writeError(w, protocol.Errorf(protocol.CodeHermesUnavailable, "search needs the agents helper"), 0)
	case st == http.StatusOK && json.Valid(b):
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(st)
		_, _ = w.Write(b)
	case st >= 400 && st < 500:
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "%s", helperMessage(b, "the search was refused")), 0)
	default:
		writeError(w, protocol.Errorf(protocol.CodeHermesError, "%s", helperMessage(b, "the search failed")), 0)
	}
}

// helperMessage is the helper's error message, or def.
func helperMessage(b []byte, def string) string {
	var out struct {
		Error struct{ Message string } `json:"error"`
	}
	if json.Unmarshal(b, &out) != nil || out.Error.Message == "" {
		return def
	}
	return out.Error.Message
}
