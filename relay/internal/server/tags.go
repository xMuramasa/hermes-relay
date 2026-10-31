package server

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

const (
	maxTags   = 10 // per session
	maxTagLen = 32 // characters
)

// normalizeTags trims and checks tags and drops case-insensitive duplicates, keeping the first spelling.
func normalizeTags(in []string) ([]string, *protocol.Error) {
	out, seen := []string{}, map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || utf8.RuneCountInString(t) > maxTagLen || !utf8.ValidString(t) || strings.ContainsFunc(t, unicode.IsControl) {
			return nil, protocol.Errorf(protocol.CodeBadRequest, "a tag must be 1-%d characters, without control characters", maxTagLen)
		}
		if k := strings.ToLower(t); !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	if len(out) > maxTags {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "at most %d tags per session", maxTags)
	}
	return out, nil
}

// listTags answers every session's tags. Ids of sessions deleted elsewhere may linger; apps ignore them.
func (s *Server) listTags(w http.ResponseWriter, r *http.Request, _ store.Device) {
	tags, err := s.store.SessionTags(r.Context())
	if err != nil {
		s.log.Error("reading tags failed", "err", err.Error())
		writeError(w, protocol.Errorf(protocol.CodeInternal, "internal error"), 0)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": tags})
}

// setTags replaces a session's tags.
func (s *Server) setTags(w http.ResponseWriter, r *http.Request, dev store.Device) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, err, 0)
		return
	}
	if body.Tags == nil {
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "tags is required ([] clears them)"), 0)
		return
	}
	tags, perr := normalizeTags(body.Tags)
	if perr != nil {
		writeError(w, perr, 0)
		return
	}
	if _, ok := s.sessionSource(w, r, id); !ok {
		return
	}
	if err := s.store.SetSessionTags(r.Context(), id, tags); err != nil {
		s.log.Error("setting tags failed", "session_id", id, "err", err.Error())
		writeError(w, protocol.Errorf(protocol.CodeInternal, "internal error"), 0)
		return
	}
	s.log.Info("session tags set", "session_id", id, "device", dev.Name, "count", len(tags))
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}
