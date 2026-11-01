// Package agents talks to the host helper trismegistus-agents (agents/ in this repo) over its Unix socket.
package agents

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

const maxBody = 4 << 20

// Client calls the helper.
type Client struct{ hc *http.Client }

// New makes a client for the helper's socket.
func New(socket string) *Client {
	return &Client{hc: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

// Do sends one request (path like /agents/run_x/transcript) and returns the helper's status and body. An error means
// the helper could not be reached.
func (c *Client) Do(ctx context.Context, method, path string, q url.Values) (int, []byte, error) {
	u := url.URL{Scheme: "http", Host: "agents", Path: path, RawQuery: q.Encode()}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return 0, nil, err
	}
	if len(b) > maxBody {
		return 0, nil, fmt.Errorf("answer over %d bytes", maxBody)
	}
	return resp.StatusCode, b, nil
}
