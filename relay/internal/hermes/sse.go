package hermes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// MaxFrame caps one SSE frame (a long final answer fits; anything bigger is a broken stream).
const MaxFrame = 8 << 20

// Frame is one Hermes run event: `data: {event, run_id, timestamp, seq, ...}`.
type Frame struct {
	Event     string
	RunID     string
	Seq       int64 // -1 when the frame has no seq
	Timestamp float64
	Fields    map[string]json.RawMessage
}

// ParseFrame decodes one SSE data payload.
func ParseFrame(data []byte) (Frame, error) {
	f := Frame{Seq: -1}
	if err := json.Unmarshal(data, &f.Fields); err != nil || f.Fields == nil {
		return f, fmt.Errorf("frame is not a JSON object")
	}
	_ = json.Unmarshal(f.Fields["event"], &f.Event)
	_ = json.Unmarshal(f.Fields["run_id"], &f.RunID)
	_ = json.Unmarshal(f.Fields["timestamp"], &f.Timestamp)
	if raw, ok := f.Fields["seq"]; ok {
		var n json.Number
		if json.Unmarshal(raw, &n) == nil {
			if v, err := n.Int64(); err == nil && v >= 0 {
				f.Seq = v
			}
		}
	}
	return f, nil
}

// Events reads GET /v1/runs/{id}/events until onFrame returns true (the caller saw a terminal frame: stopped is
// true), the stream ends (ErrUnavailable), stays silent for DeadStream (ErrStreamDead) or ctx is done.
func (c *Client) Events(ctx context.Context, runID string, onFrame func(Frame) bool) (stopped bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/v1/runs/"+url.PathEscape(runID)+"/events", nil), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("%w: event stream", ErrUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, &APIError{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}

	var dead atomic.Bool
	watchdog := time.AfterFunc(c.DeadStream, func() { dead.Store(true); cancel() })
	defer watchdog.Stop()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), MaxFrame)
	var data bytes.Buffer
	for sc.Scan() {
		watchdog.Reset(c.DeadStream)
		line := bytes.TrimSuffix(sc.Bytes(), []byte("\r"))
		switch {
		case len(line) == 0:
			if data.Len() == 0 {
				continue
			}
			f, perr := ParseFrame(data.Bytes())
			data.Reset()
			if perr != nil {
				continue // malformed frame: skip it, keep the stream
			}
			if onFrame(f) {
				return true, nil
			}
		case line[0] == ':':
			// comment: ": open", ": keepalive"
		case bytes.HasPrefix(line, []byte("data:")):
			v := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			if data.Len()+len(v) > MaxFrame {
				return false, fmt.Errorf("%w: frame too large", ErrUnavailable)
			}
			data.Write(v)
		default:
			// event:, id:, retry: lines are not used by Hermes' run stream.
		}
	}
	if watchdog.Stop(); dead.Load() {
		return false, ErrStreamDead
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, fmt.Errorf("%w: event stream ended", ErrUnavailable)
}
