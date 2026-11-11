// Package agents talks to the host helper trismegistus-agents (agents/ in this repo) over its Unix socket.
package agents

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxBody = 4 << 20

// Client calls the helper.
type Client struct{ hc, slow *http.Client }

// New makes a client for the helper's socket.
func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	// slow is for rewinds: the helper gives Hermes' interpreter up to 20 s.
	return &Client{hc: &http.Client{Timeout: 10 * time.Second, Transport: tr}, slow: &http.Client{Timeout: 25 * time.Second, Transport: tr}}
}

// MaxMedia caps an image from GET /media (the helper's own cap).
const MaxMedia = 10 << 20

// Do sends one request (path like /agents/run_x/transcript) and returns the helper's status and body. An error means
// the helper could not be reached.
func (c *Client) Do(ctx context.Context, method, path string, q url.Values) (int, []byte, error) {
	st, _, b, err := c.send(ctx, c.hc, method, path, q, nil, maxBody)
	return st, b, err
}

// Rewind asks the helper to rewind a conversation to just before its user message before (POST
// /sessions/{id}/rewind) and returns the helper's status and body. The session id must be protocol.ValidID.
func (c *Client) Rewind(ctx context.Context, sessionID string, before int64) (int, []byte, error) {
	body := strings.NewReader(`{"before_message_id": ` + strconv.FormatInt(before, 10) + "}")
	st, _, b, err := c.send(ctx, c.slow, http.MethodPost, "/sessions/"+sessionID+"/rewind", nil, body, maxBody)
	return st, b, err
}

// Media asks the helper for an image file (GET /media?path=…) and returns its status, Content-Type and bytes.
func (c *Client) Media(ctx context.Context, path string) (int, string, []byte, error) {
	return c.send(ctx, c.hc, http.MethodGet, "/media", url.Values{"path": {path}}, nil, MaxMedia)
}

func (c *Client) send(ctx context.Context, hc *http.Client, method, path string, q url.Values, body io.Reader, limit int) (int, string, []byte, error) {
	u := url.URL{Scheme: "http", Host: "agents", Path: path, RawQuery: q.Encode()}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return 0, "", nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return 0, "", nil, err
	}
	if len(b) > limit {
		return 0, "", nil, fmt.Errorf("answer over %d bytes", limit)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), b, nil
}
