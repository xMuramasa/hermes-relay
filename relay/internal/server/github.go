package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xMuramasa/trismegistus/relay/internal/github"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// SetGitHub turns on the /api/github endpoints; without it they answer not_configured. Call it before serving.
func (s *Server) SetGitHub(c *github.Client) { s.github = c }

// GitHub's error codes are REST-only, like forbidden.
var githubStatus = map[string]int{
	github.CodeNotConfigured: http.StatusServiceUnavailable,
	github.CodeRateLimited:   http.StatusTooManyRequests,
	github.CodeNotFound:      http.StatusNotFound,
	github.CodeForbidden:     http.StatusForbidden,
	github.CodeBadRequest:    http.StatusBadRequest,
	github.CodeGitHubError:   http.StatusBadGateway,
}

// githubFailed writes {"error": {code, message, retry_at?}}; GitHub's status is logged, never its text.
func (s *Server) githubFailed(w http.ResponseWriter, err error) {
	e := github.AsError(err)
	if e.Status != 0 {
		s.log.Warn("github refused", "github_status", e.Status, "code", e.Code)
	}
	body := map[string]string{"code": e.Code, "message": e.Message}
	if !e.RetryAt.IsZero() {
		body["retry_at"] = e.RetryAt.UTC().Format(time.RFC3339)
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(time.Until(e.RetryAt).Seconds()))))
	}
	writeJSON(w, githubStatus[e.Code], map[string]any{"error": body})
}

// gh answers not_configured when the relay has no GitHub token.
func (s *Server) gh(next func(http.ResponseWriter, *http.Request, store.Device)) func(http.ResponseWriter, *http.Request, store.Device) {
	return func(w http.ResponseWriter, r *http.Request, dev store.Device) {
		if s.github == nil {
			s.githubFailed(w, &github.Error{Code: github.CodeNotConfigured,
				Message: "GitHub is not set up on the relay (GITHUB_TOKEN, GITHUB_OWNER, GITHUB_PROJECT)"})
			return
		}
		next(w, r, dev)
	}
}

// ghWrite is gh for manager-only routes.
func (s *Server) ghWrite(next func(http.ResponseWriter, *http.Request, store.Device)) http.HandlerFunc {
	return s.logged(s.authed(requireRole(store.RoleManager, s.gh(next))))
}

// issueNumber reads the {number} path segment.
func issueNumber(r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("number"))
	return n, err == nil && n > 0 && n < 1<<31
}

// issuePath reads {owner}, {repo} and {number}; the client checks the owner and repo.
func (s *Server) issuePath(w http.ResponseWriter, r *http.Request) (owner, repo string, number int, ok bool) {
	number, ok = issueNumber(r)
	if !ok {
		s.githubFailed(w, &github.Error{Code: github.CodeNotFound, Message: "not found"})
	}
	return r.PathValue("owner"), r.PathValue("repo"), number, ok
}

func (s *Server) githubBoard(w http.ResponseWriter, r *http.Request, _ store.Device) {
	b, err := s.github.Board(r.Context())
	if err != nil {
		s.githubFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) githubRepos(w http.ResponseWriter, r *http.Request, _ store.Device) {
	repos, err := s.github.Repos(r.Context())
	if err != nil {
		s.githubFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

func (s *Server) githubIssues(w http.ResponseWriter, r *http.Request, _ store.Device) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	l, err := s.github.Issues(r.Context(), r.PathValue("owner"), r.PathValue("repo"), state)
	if err != nil {
		s.githubFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) githubIssue(w http.ResponseWriter, r *http.Request, _ store.Device) {
	owner, repo, n, ok := s.issuePath(w, r)
	if !ok {
		return
	}
	is, err := s.github.Issue(r.Context(), owner, repo, n)
	if err != nil {
		s.githubFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, is)
}

func (s *Server) githubComment(w http.ResponseWriter, r *http.Request, dev store.Device) {
	owner, repo, n, ok := s.issuePath(w, r)
	if !ok {
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, err, 0)
		return
	}
	if strings.TrimSpace(body.Body) == "" || !utf8.ValidString(body.Body) {
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "body must be non-empty text"), 0)
		return
	}
	c, err := s.github.AddComment(r.Context(), owner, repo, n, body.Body)
	if err != nil {
		s.githubFailed(w, err)
		return
	}
	s.log.Info("github comment added", "repo", repo, "issue", n, "device", dev.Name)
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) githubSetState(w http.ResponseWriter, r *http.Request, dev store.Device) {
	owner, repo, n, ok := s.issuePath(w, r)
	if !ok {
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, err, 0)
		return
	}
	if err := s.github.SetState(r.Context(), owner, repo, n, body.State); err != nil {
		s.githubFailed(w, err)
		return
	}
	s.log.Info("github issue state set", "repo", repo, "issue", n, "state", body.State, "device", dev.Name)
	writeJSON(w, http.StatusOK, map[string]string{"state": body.State})
}

func (s *Server) githubSetBoard(w http.ResponseWriter, r *http.Request, dev store.Device) {
	owner, repo, n, ok := s.issuePath(w, r)
	if !ok {
		return
	}
	var body struct {
		Status   *string `json:"status"`
		Priority *string `json:"priority"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, err, 0)
		return
	}
	if body.Status == nil && body.Priority == nil {
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "send status and/or priority"), 0)
		return
	}
	v, err := s.github.SetBoard(r.Context(), owner, repo, n, body.Status, body.Priority)
	if err != nil {
		s.githubFailed(w, err)
		return
	}
	s.log.Info("github board fields set", "repo", repo, "issue", n, "device", dev.Name)
	writeJSON(w, http.StatusOK, v)
}
