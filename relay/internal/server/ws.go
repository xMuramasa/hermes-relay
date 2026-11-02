package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// wsConn is one authenticated app socket. It's a hub.Subscriber.
type wsConn struct {
	srv  *Server
	c    *websocket.Conn
	dev  store.Device
	send chan []byte
	work chan struct{}  // in-flight request slots
	jobs sync.WaitGroup // in-flight requests (async); the socket is finished only after them

	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	code     websocket.StatusCode
	reason   string
	finished chan struct{} // closed when the socket is fully torn down

	subs map[string]bool // reader goroutine only
}

// Deliver queues a message; a socket that can't keep up is closed (the app resubscribes with after_seq).
func (w *wsConn) Deliver(msg []byte) {
	select {
	case w.send <- msg:
	default:
		w.shut(websocket.StatusPolicyViolation, "too slow")
	}
}

func (w *wsConn) deliver(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	w.Deliver(b)
}

func (w *wsConn) sendError(e *protocol.Error, ref string) {
	w.deliver(protocol.NewError(e.Code, e.Message, ref))
}

// shut closes the socket once with code and reason.
func (w *wsConn) shut(code websocket.StatusCode, reason string) {
	w.once.Do(func() {
		w.code, w.reason = code, reason
		w.cancel()
	})
}

func (s *Server) handleWS(rw http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.opts.TrustedProxies)
	// No Origin check: the app is not a browser page and authenticates with a token in its first message, not a
	// cookie, so a cross-site page gains nothing from opening a socket.
	c, err := websocket.Accept(rw, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(protocol.MaxMessageBytes)

	dev, ok := s.wsAuth(c)
	if !ok {
		s.log.Warn("websocket auth failed", "client_ip", ip.String())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &wsConn{
		srv: s, c: c, dev: dev, ctx: ctx, cancel: cancel,
		send:     make(chan []byte, s.opts.SendQueue),
		work:     make(chan struct{}, s.opts.MaxInFlight),
		finished: make(chan struct{}),
		subs:     map[string]bool{},
	}
	defer close(w.finished)
	if !s.register(w) {
		_ = c.Close(websocket.StatusGoingAway, "relay restarting")
		return
	}
	defer s.unregister(w)
	start := time.Now()
	s.log.Info("websocket connected", "device", dev.Name, "client_ip", ip.String())

	w.deliver(protocol.Ready{Type: "ready", Device: dev.Name, ServerVersion: s.opts.Version})
	var wg sync.WaitGroup
	wg.Go(w.writer)
	wg.Go(w.pinger)
	w.reader()
	w.shut(websocket.StatusNormalClosure, "")
	for sid := range w.subs {
		s.hub.Unsubscribe(w, sid)
	}
	wg.Wait()
	// A prompt may be inside CreateRun: shutdown must not close the store before its run is stored.
	w.jobs.Wait()
	s.log.Info("websocket closed", "device", dev.Name, "code", int(w.code), "reason", w.reason,
		"duration_ms", time.Since(start).Milliseconds())
}

// wsAuth waits for the auth message. On failure the socket is closed.
func (s *Server) wsAuth(c *websocket.Conn) (store.Device, bool) {
	timer := time.AfterFunc(s.opts.AuthTimeout, func() {
		_ = c.Close(websocket.StatusPolicyViolation, "auth timeout")
	})
	defer timer.Stop()
	typ, data, err := c.Read(context.Background())
	if err != nil {
		_ = c.CloseNow()
		return store.Device{}, false
	}
	timer.Stop()
	fail := func(code, msg string) (store.Device, bool) {
		b, _ := json.Marshal(protocol.NewError(code, msg, ""))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Write(ctx, websocket.MessageText, b)
		_ = c.Close(websocket.StatusPolicyViolation, "unauthorized")
		return store.Device{}, false
	}
	var in protocol.Inbound
	if typ != websocket.MessageText || s.validator.Validate(data) != nil || json.Unmarshal(data, &in) != nil || in.Type != "auth" {
		return fail(protocol.CodeUnauthorized, "the first message must be auth")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dev, ok := s.deviceFromToken(ctx, in.Token)
	if !ok {
		return fail(protocol.CodeUnauthorized, "unauthorized")
	}
	return dev, true
}

// register adds a socket, closing the device's oldest ones beyond the limit.
func (s *Server) register(w *wsConn) bool {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return false
	}
	cs := append(s.conns[w.dev.ID], w)
	var evicted []*wsConn
	for len(cs) > s.opts.MaxSocketsPerDevice {
		evicted = append(evicted, cs[0])
		cs = cs[1:]
	}
	s.conns[w.dev.ID] = cs
	s.mu.Unlock()
	for _, old := range evicted {
		old.shut(websocket.StatusPolicyViolation, "replaced by a newer connection")
	}
	return true
}

func (s *Server) unregister(w *wsConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.conns[w.dev.ID]
	for i, c := range cs {
		if c == w {
			cs = append(cs[:i:i], cs[i+1:]...)
			break
		}
	}
	if len(cs) == 0 {
		delete(s.conns, w.dev.ID)
	} else {
		s.conns[w.dev.ID] = cs
	}
}

func (w *wsConn) writer() {
	for {
		select {
		case <-w.ctx.Done():
			_ = w.c.Close(w.code, w.reason)
			return
		case msg := <-w.send:
			ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
			err := w.c.Write(ctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				w.shut(websocket.StatusAbnormalClosure, "write failed")
				_ = w.c.CloseNow()
				return
			}
		}
	}
}

// pinger drops the socket after two missed pongs.
func (w *wsConn) pinger() {
	t := time.NewTicker(w.srv.opts.PingInterval)
	defer t.Stop()
	missed := 0
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(w.ctx, w.srv.opts.PingInterval)
			err := w.c.Ping(ctx)
			cancel()
			if w.ctx.Err() != nil {
				return
			}
			if err == nil {
				missed = 0
				continue
			}
			if missed++; missed >= 2 {
				w.shut(websocket.StatusPolicyViolation, "ping timeout")
				return
			}
		}
	}
}

func (w *wsConn) reader() {
	for {
		// Not w.ctx: cancelling a read tears the connection down without a close frame. shut makes the writer
		// send the close frame, which ends this read.
		typ, data, err := w.c.Read(context.Background())
		if err != nil {
			if code := websocket.CloseStatus(err); code != -1 {
				w.shut(code, "")
			} else if errors.Is(err, websocket.ErrMessageTooBig) {
				w.shut(websocket.StatusMessageTooBig, "message too big")
			}
			return
		}
		if typ != websocket.MessageText {
			w.sendError(protocol.Errorf(protocol.CodeBadRequest, "messages must be JSON text"), "")
			continue
		}
		if err := w.srv.validator.Validate(data); err != nil {
			var pe *protocol.Error
			errors.As(err, &pe)
			w.sendError(pe, "")
			continue
		}
		var in protocol.Inbound
		if err := json.Unmarshal(data, &in); err != nil {
			w.sendError(protocol.Errorf(protocol.CodeBadRequest, "not valid JSON"), "")
			continue
		}
		w.handle(in)
	}
}

func (w *wsConn) handle(in protocol.Inbound) {
	s := w.srv
	switch in.Type {
	case "auth":
		w.sendError(protocol.Errorf(protocol.CodeBadRequest, "already authenticated"), "")
	case "ping":
		w.deliver(protocol.Pong{Type: "pong"})
	case "subscribe":
		if !w.subs[in.SessionID] && len(w.subs) >= s.opts.MaxSubscriptions {
			w.sendError(protocol.Errorf(protocol.CodeBadRequest, "too many subscriptions on this socket"), in.SessionID)
			return
		}
		if err := s.hub.Subscribe(w.ctx, w, in.SessionID, in.AfterSeq); err != nil {
			w.sendError(asProtocol(err), in.SessionID)
			return
		}
		w.subs[in.SessionID] = true
	case "unsubscribe":
		if w.subs[in.SessionID] {
			s.hub.Unsubscribe(w, in.SessionID)
			delete(w.subs, in.SessionID)
		}
	case "prompt":
		if !s.prompts.Allow(strconv.FormatInt(w.dev.ID, 10)) {
			w.sendError(protocol.Errorf(protocol.CodeRateLimited, "too many prompts; wait a minute"), in.ClientMsgID)
			return
		}
		w.async(in.ClientMsgID, func(ctx context.Context) error {
			_ = s.store.TouchDevice(ctx, w.dev.ID)
			return s.hub.Prompt(ctx, w.dev.ID, in.ClientMsgID, in.SessionID, in.Text, in.Attachments, func(runID string, replayed bool) {
				w.deliver(protocol.Ack{Type: "ack", ClientMsgID: in.ClientMsgID, RunID: runID, Replayed: replayed})
			})
		})
	case "stop":
		w.async(in.RunID, func(ctx context.Context) error { return s.hub.Stop(ctx, w, in.RunID) })
	case "steer":
		w.async(in.RunID, func(ctx context.Context) error { return s.hub.Steer(ctx, w, in.RunID, in.Text) })
	case "approve":
		w.async(in.RunID, func(ctx context.Context) error {
			return s.hub.Approve(ctx, w, in.RunID, in.RequestID, in.Choice)
		})
	default:
		w.sendError(protocol.Errorf(protocol.CodeBadRequest, "the relay does not accept that message type"), "")
	}
}

// async runs a request that may wait on Hermes without blocking the reader (pongs are read there).
func (w *wsConn) async(ref string, fn func(context.Context) error) {
	select {
	case w.work <- struct{}{}:
	default:
		w.sendError(protocol.Errorf(protocol.CodeRateLimited, "too many requests in flight"), ref)
		return
	}
	w.jobs.Go(func() {
		defer func() { <-w.work }()
		if err := fn(w.ctx); err != nil {
			w.sendError(asProtocol(err), ref)
		}
	})
}

func asProtocol(err error) *protocol.Error {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe
	}
	return hermes.ProtocolError(err)
}
