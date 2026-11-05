package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/xMuramasa/trismegistus/relay/internal/github"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// The helper's stream after the open line (term/README.md): one type byte, a big-endian uint16 length, the payload.
const (
	termData   byte = 0
	termResize byte = 1
	termExit   byte = 2

	termMaxFrame      = 32 << 10
	termMaxSize       = 1000 // cols, rows
	termHelperTimeout = 5 * time.Second
	codeUnavailable   = "unavailable" // /api/terminal only, like not_configured and forbidden (protocol/terminal.schema.json)
)

// SetTerminal names the host helper's socket; empty, /api/terminal answers not_configured. Call it before serving.
func (s *Server) SetTerminal(socket string) { s.termSocket = socket }

type termSize struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func parseTermSize(data []byte, typ string) (termSize, bool) {
	var m termSize
	ok := json.Unmarshal(data, &m) == nil && m.Type == typ && m.Cols >= 1 && m.Cols <= termMaxSize && m.Rows >= 1 && m.Rows <= termMaxSize
	return m, ok
}

// termAllowed is the access rule: a manager with the grant. TerminalDevices is the same rule for open terminals.
func termAllowed(dev store.Device) *protocol.Error {
	switch {
	case dev.Role != store.RoleManager:
		return protocol.Errorf(codeForbidden, "this device is a viewer; the terminal needs a manager with the grant (devices set-role %s manager, devices terminal %s on)", dev.Name, dev.Name)
	case !dev.Terminal:
		return protocol.Errorf(codeForbidden, "this device has no terminal grant; on the server: devices terminal %s on", dev.Name)
	}
	return nil
}

// handleTerminal streams one shell on the host between the app and the helper. It shares wsAuth and the ping with
// /ws, not the session protocol or the socket limit.
func (s *Server) handleTerminal(rw http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.opts.TrustedProxies)
	// No Origin check, as on /ws: the token comes in the first message, never a cookie.
	c, err := websocket.Accept(rw, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(termMaxFrame)
	dev, ok := s.wsAuth(c)
	if !ok {
		s.log.Warn("terminal auth failed", "client_ip", ip.String())
		return
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), s.opts.AuthTimeout)
	typ, data, err := c.Read(readCtx)
	readCancel()
	if err != nil {
		_ = c.CloseNow()
		return
	}
	size, ok := parseTermSize(data, "open")
	if typ != websocket.MessageText || !ok {
		s.termRefuse(c, dev, protocol.Errorf(protocol.CodeBadRequest, "the second message must be open {cols, rows}, each 1-%d", termMaxSize))
		return
	}
	if e := termAllowed(dev); e != nil {
		s.termRefuse(c, dev, e)
		return
	}
	if s.termSocket == "" {
		s.termRefuse(c, dev, protocol.Errorf(github.CodeNotConfigured, "the relay has no TERM_SOCKET"))
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &wsConn{srv: s, c: c, dev: dev, ctx: ctx, cancel: cancel, finished: make(chan struct{})}
	defer close(w.finished)
	if !s.registerTerm(w) {
		_ = c.Close(websocket.StatusGoingAway, "relay restarting")
		return
	}
	defer s.unregisterTerm(w)
	conn, br, opened, e := s.termDial(ctx, dev.Name, size)
	if e != nil {
		s.termRefuse(c, dev, e)
		return
	}
	defer conn.Close()
	b, _ := json.Marshal(opened)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		_ = c.CloseNow()
		return
	}
	start := time.Now()
	s.log.Info("terminal opened", "device", dev.Name, "client_ip", ip.String())

	var wg sync.WaitGroup
	wg.Go(w.pinger)
	wg.Go(func() { // shut lands here: closing the helper's connection is what ends the shell
		<-ctx.Done()
		conn.Close()
		_ = c.Close(w.code, w.reason)
	})
	status := -1
	wg.Go(func() { status = w.termOutput(br) })
	w.termInput(conn)
	w.shut(websocket.StatusNormalClosure, "")
	wg.Wait()
	s.log.Info("terminal closed", "device", dev.Name, "code", int(w.code), "reason", w.reason, "exit_status", status,
		"duration_ms", time.Since(start).Milliseconds())
}

// termRefuse answers with an error and closes.
func (s *Server) termRefuse(c *websocket.Conn, dev store.Device, e *protocol.Error) {
	s.log.Warn("terminal refused", "device", dev.Name, "code", e.Code)
	b, _ := json.Marshal(protocol.NewError(e.Code, e.Message, ""))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, b)
	_ = c.Close(websocket.StatusPolicyViolation, e.Code)
}

type termOpened struct {
	Type  string `json:"type"`
	Host  string `json:"host"`
	Shell string `json:"shell"`
}

// termDial asks the helper for a shell.
func (s *Server) termDial(ctx context.Context, device string, size termSize) (net.Conn, *bufio.Reader, termOpened, *protocol.Error) {
	unavailable := protocol.Errorf(codeUnavailable, "the terminal helper isn't running")
	dctx, cancel := context.WithTimeout(ctx, termHelperTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "unix", s.termSocket)
	if err != nil {
		return nil, nil, termOpened{}, unavailable
	}
	_ = conn.SetDeadline(time.Now().Add(termHelperTimeout))
	b, _ := json.Marshal(map[string]any{"device": device, "cols": size.Cols, "rows": size.Rows})
	br := bufio.NewReaderSize(conn, termMaxFrame)
	var answer struct {
		Host, Shell, Error string
	}
	_, err = conn.Write(append(b, '\n'))
	if err == nil {
		var line []byte
		if line, err = br.ReadSlice('\n'); err == nil {
			err = json.Unmarshal(line, &answer)
		}
	}
	switch {
	case err == nil && answer.Error == "":
		_ = conn.SetDeadline(time.Time{})
		return conn, br, termOpened{Type: "opened", Host: answer.Host, Shell: answer.Shell}, nil
	case err == nil && answer.Error == protocol.CodeBusy:
		conn.Close()
		return nil, nil, termOpened{}, protocol.Errorf(protocol.CodeBusy, "the host has 4 terminals open")
	}
	conn.Close()
	return nil, nil, termOpened{}, unavailable
}

func writeTermFrame(w io.Writer, typ byte, p []byte) error {
	b := make([]byte, 3+len(p))
	b[0] = typ
	binary.BigEndian.PutUint16(b[1:], uint16(len(p)))
	copy(b[3:], p)
	_, err := w.Write(b)
	return err
}

// readTermFrame reads one frame into buf (termMaxFrame long).
func readTermFrame(r io.Reader, buf []byte) (byte, []byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[1:]))
	if n > len(buf) {
		return 0, nil, errors.New("terminal frame too long")
	}
	_, err := io.ReadFull(r, buf[:n])
	return hdr[0], buf[:n], err
}

// termOutput copies helper frames to the app one at a time, nothing queued: a slow app stops this read, which stops
// the helper and then the shell. It returns the shell's exit status, -1 if the stream ended without one.
func (w *wsConn) termOutput(br *bufio.Reader) int {
	buf := make([]byte, termMaxFrame)
	for {
		typ, p, err := readTermFrame(br, buf)
		if err != nil {
			w.shut(websocket.StatusGoingAway, "terminal helper gone")
			return -1
		}
		switch {
		case typ == termData:
			if err := w.c.Write(w.ctx, websocket.MessageBinary, p); err != nil {
				w.shut(websocket.StatusAbnormalClosure, "write failed")
				return -1
			}
		case typ == termExit && len(p) == 4:
			status := int(int32(binary.BigEndian.Uint32(p)))
			_ = w.c.Write(w.ctx, websocket.MessageText, fmt.Appendf(nil, `{"type":"exit","status":%d}`, status))
			w.shut(websocket.StatusNormalClosure, "")
			return status
		}
	}
}

// termInput copies app frames to the helper one at a time: a shell that stops reading stops this read, and the app's
// socket fills up.
func (w *wsConn) termInput(conn net.Conn) {
	for {
		// Not w.ctx, as in reader: shut's close frame ends this read.
		typ, data, err := w.c.Read(context.Background())
		if err != nil {
			if code := websocket.CloseStatus(err); code != -1 {
				w.shut(code, "")
			} else if errors.Is(err, websocket.ErrMessageTooBig) {
				w.shut(websocket.StatusMessageTooBig, "message too big")
			}
			return
		}
		frame, payload := termData, data
		if typ != websocket.MessageBinary {
			size, ok := parseTermSize(data, "resize")
			if !ok {
				b, _ := json.Marshal(protocol.NewError(protocol.CodeBadRequest, "text frames must be resize {cols, rows}", ""))
				_ = w.c.Write(w.ctx, websocket.MessageText, b)
				continue
			}
			frame, payload = termResize, binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, uint16(size.Cols)), uint16(size.Rows))
		}
		if err := writeTermFrame(conn, frame, payload); err != nil {
			w.shut(websocket.StatusGoingAway, "terminal helper gone")
			return
		}
	}
}

func (s *Server) registerTerm(w *wsConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.terms[w] = true
	return true
}

func (s *Server) unregisterTerm(w *wsConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.terms, w)
}

// dropTerminals closes the terminals of devices that may no longer have one: revoked, demoted or without the grant.
func (s *Server) dropTerminals(ctx context.Context) {
	ids, err := s.store.TerminalDevices(ctx)
	if err != nil {
		return
	}
	s.mu.Lock()
	var gone []*wsConn
	for w := range s.terms {
		if !ids[w.dev.ID] {
			gone = append(gone, w)
		}
	}
	s.mu.Unlock()
	for _, w := range gone {
		s.log.Info("terminal access ended, closing", "device", w.dev.Name)
		w.shut(websocket.StatusPolicyViolation, "terminal access ended")
	}
}
