package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// A conversation's images that live on the host (#26): Hermes' MEDIA:<path> lines in answers and Telegram's
// "[Image attached at: <path>]" notes. The relay serves a path only if it occurs in that session's messages, and gets
// the bytes from the agents helper, which applies its own path rules.

const (
	mediaTTL      = 60 * time.Second
	mediaPageSize = 500
	mediaPages    = 20 // shortcut: paths in messages past the 10000th are not served; page further if a session gets there
)

var (
	mediaTagRe   = regexp.MustCompile(`MEDIA:\s*(/\S+)`)
	imageNoteRe  = regexp.MustCompile(`\[Image attached at: (/[^\]\n]+)\]`)
	mediaTrailer = "\"'`.,;:)]}>"
)

type mediaCache struct {
	mu sync.Mutex
	m  map[string]mediaEntry // by session id
}

type mediaEntry struct {
	at    time.Time
	paths map[string]bool
}

// mediaPaths returns the image paths a session's messages name, cached for mediaTTL.
func (s *Server) mediaPaths(ctx context.Context, id string) (map[string]bool, error) {
	s.media.mu.Lock()
	e, ok := s.media.m[id]
	s.media.mu.Unlock()
	if ok && time.Since(e.at) < mediaTTL {
		return e.paths, nil
	}
	paths := map[string]bool{}
	for page := range mediaPages {
		q := url.Values{"limit": {strconv.Itoa(mediaPageSize)}, "offset": {strconv.Itoa(page * mediaPageSize)}}
		resp, err := s.hermes.Do(ctx, http.MethodGet, "/api/sessions/"+url.PathEscape(id)+"/messages", q, nil, nil)
		if err != nil {
			return nil, err
		}
		var list struct {
			Data []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal(resp.Body, &list) != nil {
			return nil, &hermes.APIError{Status: http.StatusBadGateway}
		}
		for _, m := range list.Data {
			text, _ := m.Content.(string)
			re := map[string]*regexp.Regexp{"assistant": mediaTagRe, "user": imageNoteRe}[m.Role]
			if re == nil {
				continue
			}
			for _, sub := range re.FindAllStringSubmatch(text, -1) {
				paths[strings.TrimRight(strings.TrimSpace(sub[1]), mediaTrailer)] = true
			}
		}
		if len(list.Data) < mediaPageSize {
			break
		}
	}
	s.media.mu.Lock()
	if s.media.m == nil || len(s.media.m) >= 1000 {
		s.media.m = map[string]mediaEntry{}
	}
	s.media.m[id] = mediaEntry{at: time.Now(), paths: paths}
	s.media.mu.Unlock()
	return paths, nil
}

// sessionMedia serves GET /api/sessions/{id}/media?path=…: 404 for any path the session's messages don't name, so
// the answer never tells whether a file exists.
func (s *Server) sessionMedia(w http.ResponseWriter, r *http.Request, _ store.Device) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	notFound := protocol.Errorf(protocol.CodeNotFound, "not found")
	p := r.URL.Query().Get("path")
	if !strings.HasPrefix(p, "/") || len(p) > 4096 {
		writeError(w, notFound, 0)
		return
	}
	paths, err := s.mediaPaths(r.Context(), id)
	if err != nil {
		writeError(w, hermes.ProtocolError(err), hermes.RetryAfterOf(err))
		return
	}
	if !paths[p] {
		writeError(w, notFound, 0)
		return
	}
	if s.agents == nil {
		writeError(w, protocol.Errorf(protocol.CodeHermesUnavailable, "the agents helper is not set up on the relay (AGENTS_SOCKET)"), 0)
		return
	}
	st, ctype, b, err := s.agents.Media(r.Context(), p)
	if err != nil {
		s.log.Warn("agents helper unreachable", "err", err.Error())
		writeError(w, protocol.Errorf(protocol.CodeHermesUnavailable, "the agents helper is not running on the host"), 0)
		return
	}
	if st != http.StatusOK || !strings.HasPrefix(ctype, "image/") {
		writeError(w, notFound, 0)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
