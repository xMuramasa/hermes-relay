package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/hub"
	"github.com/xMuramasa/trismegistus/relay/internal/pairing"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

const maxRequestBody = 16 << 10

// deviceFromToken checks a device token.
func (s *Server) deviceFromToken(ctx context.Context, token string) (store.Device, bool) {
	if !pairing.WellFormedToken(token) {
		return store.Device{}, false
	}
	dev, err := s.store.DeviceByTokenHash(ctx, pairing.Hash(token))
	if err != nil {
		return store.Device{}, false
	}
	_ = s.store.TouchDevice(ctx, dev.ID)
	return dev, true
}

// authed requires "Authorization: Bearer <device token>".
func (s *Server) authed(next func(http.ResponseWriter, *http.Request, store.Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		dev, valid := s.deviceFromToken(r.Context(), strings.TrimSpace(token))
		if !ok || !valid {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, protocol.Errorf(protocol.CodeUnauthorized, "unauthorized"), 0)
			return
		}
		next(w, r, dev)
	}
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.opts.TrustedProxies)
	if !s.pairIP.Allow(ip.String()) || !s.pairAll.Allow("") {
		s.log.Warn("pairing rate limited", "client_ip", ip.String())
		writeError(w, protocol.Errorf(protocol.CodeRateLimited, "too many pairing attempts; wait a minute"), time.Minute)
		return
	}
	var body struct {
		Code string `json:"code"`
		Name string `json:"name,omitempty"` // accepted, ignored: the name comes from `pair --name`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, err, 0)
		return
	}
	invalid := protocol.Errorf(protocol.CodeUnauthorized, "invalid, used or expired pairing code")
	code, ok := pairing.NormalizeCode(body.Code)
	if !ok {
		s.log.Warn("pairing failed", "client_ip", ip.String(), "reason", "malformed code")
		writeError(w, invalid, 0)
		return
	}
	token, tokenHash := pairing.NewToken()
	dev, err := s.store.RedeemPairCode(r.Context(), pairing.Hash(code), tokenHash, s.opts.MaxDevices)
	switch {
	case errors.Is(err, store.ErrInvalidCode):
		s.log.Warn("pairing failed", "client_ip", ip.String(), "reason", "unknown, used or expired code")
		writeError(w, invalid, 0)
		return
	case errors.Is(err, store.ErrNameExists), errors.Is(err, store.ErrTooManyDevices):
		s.log.Warn("pairing failed", "client_ip", ip.String(), "reason", err.Error())
		writeError(w, protocol.Errorf(protocol.CodeBusy, "%s; run pair again", err.Error()), 0)
		return
	case err != nil:
		s.log.Error("pairing failed", "err", err.Error())
		writeError(w, protocol.Errorf(protocol.CodeInternal, "internal error"), 0)
		return
	}
	s.log.Info("device paired", "device", dev.Name, "client_ip", ip.String())
	writeJSON(w, http.StatusOK, map[string]any{
		"token":  token,
		"device": map[string]string{"name": dev.Name, "created_at": dev.CreatedAt.UTC().Format(time.RFC3339)},
	})
}

// decodeBody reads a small JSON object with no unknown fields.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) *protocol.Error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return protocol.Errorf(protocol.CodeTooLarge, "body too large")
		}
		return protocol.Errorf(protocol.CodeBadRequest, "body must be a JSON object with the documented fields")
	}
	if dec.More() {
		return protocol.Errorf(protocol.CodeBadRequest, "body must be one JSON object")
	}
	return nil
}

var queryValueRe = regexp.MustCompile(`^[A-Za-z0-9_.,:-]{1,64}$`)

// allowQuery keeps only the listed query parameters, with plain values.
func allowQuery(q url.Values, keys ...string) url.Values {
	out := url.Values{}
	for _, k := range keys {
		if v := q.Get(k); queryValueRe.MatchString(v) {
			out.Set(k, v)
		}
	}
	return out
}

// sessionID reads and checks the {id} path segment.
func sessionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !protocol.ValidID(id) {
		writeError(w, protocol.Errorf(protocol.CodeNotFound, "not found"), 0)
		return "", false
	}
	return id, true
}

// relay writes Hermes' answer: its JSON body on success, or a protocol error.
func (s *Server) relay(w http.ResponseWriter, resp *hermes.Response, err error) bool {
	if err != nil {
		if st := hermes.StatusOf(err); st != 0 {
			s.log.Warn("hermes refused", "hermes_status", st)
		}
		writeError(w, hermes.ProtocolError(err), hermes.RetryAfterOf(err))
		return false
	}
	if len(resp.Body) == 0 {
		w.WriteHeader(resp.Status)
		return true
	}
	if !json.Valid(resp.Body) {
		writeError(w, protocol.Errorf(protocol.CodeHermesError, "Hermes sent an invalid response"), 0)
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
	return true
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request, _ store.Device) {
	resp, err := s.hermes.Do(r.Context(), http.MethodGet, "/api/sessions",
		allowQuery(r.URL.Query(), "limit", "offset", "order", "source"), nil, nil)
	if err == nil {
		for _, sess := range hermes.SessionsFrom(resp.Body) {
			s.hub.RememberSource(sess.ID, sess.Source)
		}
	}
	s.relay(w, resp, err)
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request, dev store.Device) {
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, maxRequestBody))
	// No title: Hermes wants unique titles, and it names sessions itself.
	resp, err := s.hermes.Do(r.Context(), http.MethodPost, "/api/sessions", nil, []byte("{}"), nil)
	if err == nil {
		if sess, ok := hermes.SessionFrom(resp.Body); ok {
			s.hub.RememberSource(sess.ID, sess.Source)
			s.log.Info("session created", "session_id", sess.ID, "device", dev.Name)
		}
	}
	s.relay(w, resp, err)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request, _ store.Device) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	resp, err := s.hermes.Do(r.Context(), http.MethodGet, "/api/sessions/"+url.PathEscape(id), nil, nil, nil)
	if err == nil {
		if sess, ok := hermes.SessionFrom(resp.Body); ok && sess.ID == id {
			s.hub.RememberSource(sess.ID, sess.Source)
		}
	}
	s.relay(w, resp, err)
}

func (s *Server) sessionMessages(w http.ResponseWriter, r *http.Request, _ store.Device) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	resp, err := s.hermes.Do(r.Context(), http.MethodGet, "/api/sessions/"+url.PathEscape(id)+"/messages",
		allowQuery(r.URL.Query(), "limit", "offset", "order", "before", "after"), nil, nil)
	if err == nil {
		resp.Body = s.withAttachments(r.Context(), id, resp.Body)
	}
	s.relay(w, resp, err)
}

// sessionSource looks up a session's source in Hermes (cached), which also checks it exists.
func (s *Server) sessionSource(w http.ResponseWriter, r *http.Request, id string) (string, bool) {
	src, err := s.hub.SessionSource(r.Context(), id)
	if err != nil {
		writeError(w, hermes.ProtocolError(err), hermes.RetryAfterOf(err))
		return "", false
	}
	return src, true
}

// canDelete is an allowlist of sources, so new messaging platforms stay protected; Telegram is the main chat's memory.
func canDelete(source string) bool {
	return source == hub.SourceAPIServer || source == "cron" || source == "cli"
}

// updateSession renames and/or pins any session. Only title and pinned reach Hermes.
func (s *Server) updateSession(w http.ResponseWriter, r *http.Request, dev store.Device) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	var body struct {
		Title  *string `json:"title"`
		Pinned *bool   `json:"pinned"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, err, 0)
		return
	}
	patch := map[string]any{}
	if body.Title != nil {
		title := strings.TrimSpace(*body.Title)
		if title == "" || utf8.RuneCountInString(title) > 200 || !utf8.ValidString(title) {
			writeError(w, protocol.Errorf(protocol.CodeBadRequest, "title must be 1-200 characters"), 0)
			return
		}
		patch["title"] = title
	}
	if body.Pinned != nil {
		patch["pinned"] = *body.Pinned
	}
	if len(patch) == 0 {
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "send title and/or pinned"), 0)
		return
	}
	b, _ := json.Marshal(patch)
	resp, err := s.hermes.Do(r.Context(), http.MethodPatch, "/api/sessions/"+url.PathEscape(id), nil, b, nil)
	if s.relay(w, resp, err) {
		s.log.Info("session updated", "session_id", id, "device", dev.Name)
	}
}

const turnActive = "a run is still writing to this session; try again when it ends"

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request, dev store.Device) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	src, ok := s.sessionSource(w, r, id)
	if !ok {
		return
	}
	if !canDelete(src) {
		writeError(w, protocol.Errorf(protocol.CodeReadOnly, "this session can't be deleted here"), 0)
		return
	}
	if s.hub.ActiveRun(id) != "" {
		writeError(w, protocol.Errorf(protocol.CodeBusy, turnActive), 0)
		return
	}
	resp, err := s.hermes.Do(r.Context(), http.MethodDelete, "/api/sessions/"+url.PathEscape(id), nil, nil, nil)
	// Hermes' 409 session_active_turn: a turn from another surface (cron, CLI) is writing to it.
	if hermes.StatusOf(err) == http.StatusConflict {
		s.log.Warn("hermes refused", "hermes_status", http.StatusConflict)
		writeError(w, protocol.Errorf(protocol.CodeBusy, turnActive), 0)
		return
	}
	if s.relay(w, resp, err) {
		s.hub.ForgetSource(id)
		if err := s.store.SetSessionTags(r.Context(), id, nil); err != nil {
			s.log.Error("dropping tags failed", "session_id", id, "err", err.Error())
		}
		if err := s.store.ForgetSession(r.Context(), id); err != nil {
			s.log.Error("dropping links and model failed", "session_id", id, "err", err.Error())
		}
		if err := s.store.DropSessionAttachments(r.Context(), id); err != nil {
			s.log.Error("dropping attachments failed", "session_id", id, "err", err.Error())
		}
		s.log.Info("session deleted", "session_id", id, "device", dev.Name)
	}
}
