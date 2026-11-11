// Package hermes is a small client for the parts of the Hermes API server the relay uses: sessions, runs and the
// run event stream (SSE). Everything Hermes sends is treated as untrusted and size-capped.
package hermes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
)

const (
	// MaxBody caps any REST response body read from Hermes.
	MaxBody = 16 << 20
	// DefaultRESTTimeout bounds every REST call.
	DefaultRESTTimeout = 10 * time.Second
	// DefaultDeadStream is how long an SSE stream may stay silent (no frame, no keepalive) before it's dropped.
	DefaultDeadStream = 45 * time.Second
)

// Client talks to one Hermes API server with its bearer key.
type Client struct {
	base        *url.URL
	key         string
	http        *http.Client
	RESTTimeout time.Duration
	DeadStream  time.Duration
}

// New returns a client for baseURL (e.g. http://127.0.0.1:8642).
func New(baseURL, key string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid Hermes URL")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = DefaultRESTTimeout
	return &Client{
		base:        u,
		key:         key,
		http:        &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		RESTTimeout: DefaultRESTTimeout,
		DeadStream:  DefaultDeadStream,
	}, nil
}

// APIError is a non-2xx answer from Hermes.
type APIError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *APIError) Error() string { return "hermes: HTTP " + strconv.Itoa(e.Status) }

// ErrUnavailable wraps transport failures (connection refused, timeouts, cut streams).
var ErrUnavailable = errors.New("hermes unavailable")

// ErrStreamDead is returned when an event stream stays silent longer than DeadStream.
var ErrStreamDead = errors.New("hermes event stream went silent")

// StatusOf returns the HTTP status of an *APIError, or 0.
func StatusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// RetryAfterOf returns the Retry-After of an *APIError, or 0.
func RetryAfterOf(err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.RetryAfter
	}
	return 0
}

// ProtocolError maps a client error to a protocol error for the phone.
func ProtocolError(err error) *protocol.Error {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe
	}
	switch s := StatusOf(err); {
	case s == 0:
		if errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			return protocol.Errorf(protocol.CodeHermesUnavailable, "Hermes is not reachable")
		}
		return protocol.Errorf(protocol.CodeInternal, "internal error")
	case s == http.StatusNotFound:
		return protocol.Errorf(protocol.CodeNotFound, "not found")
	case s == http.StatusConflict:
		return protocol.Errorf(protocol.CodeBusy, "Hermes refused: busy or not in a state that allows this")
	case s == http.StatusTooManyRequests:
		return protocol.Errorf(protocol.CodeRateLimited, "Hermes is rate limiting")
	case s == http.StatusServiceUnavailable || s == http.StatusBadGateway || s == http.StatusGatewayTimeout:
		msg := "Hermes is unavailable"
		if ra := RetryAfterOf(err); ra > 0 {
			msg += fmt.Sprintf("; retry after %ds", int(ra.Seconds()))
		}
		return protocol.Errorf(protocol.CodeHermesUnavailable, "%s", msg)
	case s == http.StatusBadRequest || s == http.StatusUnprocessableEntity:
		return protocol.Errorf(protocol.CodeBadRequest, "Hermes rejected the request")
	default:
		return protocol.Errorf(protocol.CodeHermesError, "Hermes error (HTTP %d)", s)
	}
}

func (c *Client) url(path string, q url.Values) string {
	u := *c.base
	u.Path = c.base.Path + path
	u.RawQuery = q.Encode()
	return u.String()
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n >= 0 {
		return min(time.Duration(n)*time.Second, time.Hour)
	}
	if t, err := http.ParseTime(h); err == nil {
		return min(max(time.Until(t), 0), time.Hour)
	}
	return 0
}

// Response is a raw REST answer.
type Response struct {
	Status int
	Body   []byte
}

// Do sends one REST request (JSON body if body != nil) and returns the status and body. Non-2xx answers return
// both the response and an *APIError.
func (c *Client) Do(ctx context.Context, method, path string, q url.Values, body []byte, hdr http.Header) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.RESTTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, q), rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s", ErrUnavailable, method, redactPath(path))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading response", ErrUnavailable)
	}
	if len(b) > MaxBody {
		return nil, &APIError{Status: http.StatusBadGateway}
	}
	r := &Response{Status: resp.StatusCode, Body: b}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return r, &APIError{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	return r, nil
}

// redactPath keeps only the route shape for error messages.
func redactPath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any, hdr http.Header) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	resp, err := c.Do(ctx, method, path, nil, body, hdr)
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return &APIError{Status: http.StatusBadGateway}
		}
	}
	return nil
}

// Session is the part of a Hermes session the relay looks at.
type Session struct {
	ID     string `json:"id"`
	Source string `json:"source"`
}

// SessionFrom extracts a session from GET/POST /api/sessions[/{id}] bodies ({session: {...}} or the bare object).
func SessionFrom(body []byte) (Session, bool) {
	var wrap struct {
		Session *Session `json:"session"`
	}
	if json.Unmarshal(body, &wrap) == nil && wrap.Session != nil && wrap.Session.ID != "" {
		return *wrap.Session, true
	}
	var s Session
	if json.Unmarshal(body, &s) == nil && s.ID != "" {
		return s, true
	}
	return Session{}, false
}

// SessionsFrom extracts id/source pairs from a GET /api/sessions body.
func SessionsFrom(body []byte) []Session {
	var list struct {
		Data []Session `json:"data"`
	}
	if json.Unmarshal(body, &list) != nil {
		return nil
	}
	return list.Data
}

// GetSession fetches one session.
func (c *Client) GetSession(ctx context.Context, id string) (Session, error) {
	resp, err := c.Do(ctx, http.MethodGet, "/api/sessions/"+url.PathEscape(id), nil, nil, nil)
	if err != nil {
		return Session{}, err
	}
	s, ok := SessionFrom(resp.Body)
	if !ok {
		return Session{}, &APIError{Status: http.StatusBadGateway}
	}
	return s, nil
}

// RunCreated is the answer to POST /v1/runs.
type RunCreated struct {
	RunID    string `json:"run_id"`
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
}

// CreateRun starts a turn. idempotencyKey makes resends return the same run.
// RunModel picks the model and reasoning effort for one run; zero fields leave them to Hermes (its defaults).
type RunModel struct {
	Provider string
	Model    string
	Effort   string // "none" turns reasoning off
}

// Image is a prompt's image, sent to Hermes as a data URL.
type Image struct {
	Mime string
	Data []byte
}

// CreateRun starts a run. A chosen model rides in the body because Hermes' /v1/runs ignores the persisted
// session lock that POST /api/sessions/{id}/model sets (only /api/sessions/{id}/chat reads it); an explicit
// provider is always honored there. With images, input is one user message of content parts (the text part
// left out when the text is empty); the data URLs exist only inside this call.
func (c *Client) CreateRun(ctx context.Context, sessionID, input, idempotencyKey string, model RunModel, images []Image) (RunCreated, error) {
	var out RunCreated
	hdr := http.Header{"Idempotency-Key": {idempotencyKey}}
	body := map[string]any{"input": input, "session_id": sessionID}
	if len(images) > 0 {
		var parts []any
		if input != "" {
			parts = append(parts, map[string]string{"type": "text", "text": input})
		}
		for _, im := range images {
			url := "data:" + im.Mime + ";base64," + base64.StdEncoding.EncodeToString(im.Data)
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
		}
		body["input"] = []any{map[string]any{"role": "user", "content": parts}}
	}
	if model.Provider != "" && model.Model != "" {
		body["provider"], body["model"] = model.Provider, model.Model
	}
	if model.Effort == "none" {
		body["model_options"] = map[string]any{"reasoning": map[string]any{"enabled": false}}
	} else if model.Effort != "" {
		body["model_options"] = map[string]any{"reasoning": map[string]any{"effort": model.Effort}}
	}
	err := c.doJSON(ctx, http.MethodPost, "/v1/runs", body, &out, hdr)
	if err == nil && !protocol.ValidID(out.RunID) {
		err = &APIError{Status: http.StatusBadGateway}
	}
	return out, err
}

// Approval is a pending approval as Hermes reports it.
type Approval struct {
	RequestID    string   `json:"request_id"`
	Command      string   `json:"command"`
	Description  string   `json:"description"`
	Choices      []string `json:"choices"`
	AllowSession *bool    `json:"allow_session"`
}

// RunStatus is the answer to GET /v1/runs/{id}.
type RunStatus struct {
	RunID          string    `json:"run_id"`
	SessionID      string    `json:"session_id"`
	Status         string    `json:"status"`
	Output         *string   `json:"output"`
	Error          any       `json:"error"`
	TurnExitReason string    `json:"turn_exit_reason"`
	Approval       *Approval `json:"approval"`
	// Fields is the whole body, so terminal states can be mapped like the matching SSE frame.
	Fields map[string]json.RawMessage `json:"-"`
}

// GetRun fetches a run's status.
func (c *Client) GetRun(ctx context.Context, runID string) (RunStatus, error) {
	var out RunStatus
	resp, err := c.Do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID), nil, nil, nil)
	if err != nil {
		return out, err
	}
	if json.Unmarshal(resp.Body, &out) != nil || json.Unmarshal(resp.Body, &out.Fields) != nil {
		return out, &APIError{Status: http.StatusBadGateway}
	}
	return out, nil
}

// ModelOptions is the part of GET /api/model/options the relay uses.
type ModelOptions struct {
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Providers []struct {
		Slug         string   `json:"slug"`
		Name         string   `json:"name"`
		Models       []string `json:"models"`
		Capabilities map[string]struct {
			Reasoning bool `json:"reasoning"`
		} `json:"capabilities"`
	} `json:"providers"`
}

// GetModelOptions lists the models of the providers Hermes has credentials for.
func (c *Client) GetModelOptions(ctx context.Context) (ModelOptions, error) {
	var out ModelOptions
	resp, err := c.Do(ctx, http.MethodGet, "/api/model/options", url.Values{"include_unconfigured": {"false"}}, nil, nil)
	if err != nil {
		return out, err
	}
	if json.Unmarshal(resp.Body, &out) != nil {
		return out, &APIError{Status: http.StatusBadGateway}
	}
	return out, nil
}

// LockSessionModel stores a per-session model lock in Hermes.
func (c *Client) LockSessionModel(ctx context.Context, sessionID, provider, model string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/sessions/"+url.PathEscape(sessionID)+"/model",
		map[string]string{"provider": provider, "model": model}, nil, nil)
}

// ErrorMessage is the text of a Hermes {"error": "..."} body (the cron jobs API's errors), capped, or "".
func ErrorMessage(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	msg := []rune(strings.ToValidUTF8(strings.TrimSpace(e.Error), ""))
	return string(msg[:min(len(msg), 300)])
}

// CronRun is one run of a cron job: the session Hermes made for it.
type CronRun struct {
	ID        string `json:"id"`
	StartedAt any    `json:"started_at"`
	Title     any    `json:"title"`
}

const cronPage, cronPages = 200, 5

// CronRuns lists a cron job's runs (its cron_<job id>_<YYYYMMDD_HHMMSS> sessions), newest first, at most limit.
// shortcut: Hermes' session list has no id filter, so this pages the 1000 most recently active cron sessions; use a
// filter if Hermes gets one.
func (c *Client) CronRuns(ctx context.Context, jobID string, limit int) ([]CronRun, error) {
	prefix := "cron_" + jobID + "_"
	seen := map[string]bool{}
	runs := []CronRun{}
	for page := range cronPages {
		q := url.Values{"source": {"cron"}, "limit": {strconv.Itoa(cronPage)}, "offset": {strconv.Itoa(page * cronPage)}}
		resp, err := c.Do(ctx, http.MethodGet, "/api/sessions", q, nil, nil)
		if err != nil {
			return nil, err
		}
		var list struct {
			Data    []CronRun `json:"data"`
			HasMore bool      `json:"has_more"`
		}
		if json.Unmarshal(resp.Body, &list) != nil {
			return nil, &APIError{Status: http.StatusBadGateway}
		}
		for _, r := range list.Data {
			// Pinned sessions are back-filled on every page, hence seen.
			if strings.HasPrefix(r.ID, prefix) && protocol.ValidID(r.ID) && !seen[r.ID] {
				seen[r.ID] = true
				runs = append(runs, r)
			}
		}
		if !list.HasMore {
			break
		}
	}
	// The id ends in the start time, so it sorts by time.
	slices.SortFunc(runs, func(a, b CronRun) int { return strings.Compare(b.ID, a.ID) })
	return runs[:min(len(runs), limit)], nil
}

// Approve answers a pending approval.
func (c *Client) Approve(ctx context.Context, runID, requestID, choice string) error {
	body := map[string]any{"request_id": requestID, "choice": choice, "execution_generation": nil}
	return c.doJSON(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/approval", body, nil, nil)
}

// Stop interrupts a run.
func (c *Client) Stop(ctx context.Context, runID string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/stop", map[string]any{}, nil, nil)
}

// Steer queues extra guidance for a running run.
func (c *Client) Steer(ctx context.Context, runID, text string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/steer", map[string]string{"text": text}, nil, nil)
}
