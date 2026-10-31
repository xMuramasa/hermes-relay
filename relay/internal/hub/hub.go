// Package hub owns the relay's live state: one active run per session, the Hermes event readers, event numbering
// and storage, and fan-out to subscribed sockets.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// Subscriber receives encoded relay → phone messages. Deliver must not block (queue or drop the subscriber).
type Subscriber interface {
	Deliver(msg []byte)
}

// Options tune run tracking.
type Options struct {
	PollMin   time.Duration // first wait between GET /v1/runs/{id} polls after a stream drop
	PollMax   time.Duration // cap between polls
	GiveUp    time.Duration // without contact for this long, the run is marked failed
	MaxReplay int           // most events one subscribe may replay; more is a replay_gap
}

// DefaultOptions are the production settings.
func DefaultOptions() Options {
	return Options{PollMin: time.Second, PollMax: 30 * time.Second, GiveUp: 2 * time.Hour, MaxReplay: 5000}
}

// Hub is safe for concurrent use.
type Hub struct {
	store  *store.Store
	hermes *hermes.Client
	log    *slog.Logger
	opts   Options

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex // guards the maps below and closed; never held while taking a session lock
	closed   bool
	sessions map[string]*session
	runs     map[string]*runState // active runs
	inflight map[string]*pending  // prompts waiting for Hermes, by client_msg_id
	sources  map[string]string    // session id → Hermes source
}

type session struct {
	id string
	mu sync.Mutex // orders event numbering and fan-out; guards the fields below
	// dead is set when the session was dropped from Hub.sessions; lockSession retries.
	dead     bool
	subs     map[Subscriber]struct{}
	active   *runState
	starting bool
}

type runState struct {
	id, sessionID string
	status        string // guarded by the session lock
	hermesSeq     int64  // tracker goroutine only
	lastApproval  string // tracker goroutine only
	started       time.Time
	events        int // guarded by the session lock
}

type pending struct {
	done  chan struct{}
	runID string
	err   error
}

// New makes a hub. Call Start to resume runs left over from a previous process.
func New(st *store.Store, hc *hermes.Client, log *slog.Logger, opts Options) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{
		store: st, hermes: hc, log: log, opts: opts, ctx: ctx, cancel: cancel,
		sessions: map[string]*session{}, runs: map[string]*runState{},
		inflight: map[string]*pending{}, sources: map[string]string{},
	}
}

// Start re-attaches every run the database still has as in progress. Each first gets a reconnected event.
func (h *Hub) Start(ctx context.Context) error {
	runs, err := h.store.ActiveRuns(ctx)
	if err != nil {
		return err
	}
	for _, r := range runs {
		rs := &runState{id: r.RunID, sessionID: r.SessionID, status: r.Status, hermesSeq: r.HermesSeq, started: time.Now()}
		s := h.lockSession(r.SessionID)
		s.active = rs
		h.mu.Lock()
		h.runs[rs.id] = rs
		h.wg.Add(1)
		h.mu.Unlock()
		s.mu.Unlock()
		h.log.Info("resuming run", "session_id", rs.sessionID, "run_id", rs.id, "hermes_seq", rs.hermesSeq)
		go h.track(rs, true)
	}
	return nil
}

// Close stops all event readers and waits for them. Runs stay in the database for the next start.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.cancel()
	h.wg.Wait()
}

// lockSession returns the live session for id, locked.
func (h *Hub) lockSession(id string) *session {
	for {
		h.mu.Lock()
		s := h.sessions[id]
		if s == nil {
			s = &session{id: id, subs: map[Subscriber]struct{}{}}
			h.sessions[id] = s
		}
		h.mu.Unlock()
		s.mu.Lock()
		if !s.dead {
			return s
		}
		s.mu.Unlock()
	}
}

// unlockSession unlocks s, dropping it from the map when nothing refers to it any more.
func (h *Hub) unlockSession(s *session) {
	if len(s.subs) == 0 && s.active == nil && !s.starting {
		s.dead = true
		h.mu.Lock()
		if h.sessions[s.id] == s {
			delete(h.sessions, s.id)
		}
		h.mu.Unlock()
	}
	s.mu.Unlock()
}

func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only protocol structs are encoded
	}
	return b
}

func eventMsg(e store.Event) []byte {
	return encode(protocol.Event{Type: "event", SessionID: e.SessionID, RunID: e.RunID, Seq: e.Seq, Kind: e.Kind, Data: e.Data, TS: e.TS})
}

func runMsg(rs *runState) []byte {
	return encode(protocol.Run{Type: "run", RunID: rs.id, SessionID: rs.sessionID, Status: rs.status})
}

// emit stores evs and the run update atomically, then fans out the events and any status change. In-memory state
// follows the update even if the database write fails, so a terminal frame always frees the session.
func (h *Hub) emit(rs *runState, evs []store.NewEvent, upd store.RunUpdate) {
	s := h.lockSession(rs.sessionID)
	defer h.unlockSession(s)
	stored, err := h.store.AppendEvents(context.Background(), rs.sessionID, rs.id, evs, upd)
	if err != nil {
		h.log.Error("storing events failed", "session_id", rs.sessionID, "run_id", rs.id, "events", len(evs), "err", err.Error())
	}
	for _, e := range stored {
		msg := eventMsg(e)
		for sub := range s.subs {
			sub.Deliver(msg)
		}
	}
	rs.events += len(stored)
	if upd.Status == "" || upd.Status == rs.status {
		return
	}
	rs.status = upd.Status
	msg := runMsg(rs)
	for sub := range s.subs {
		sub.Deliver(msg)
	}
	if protocol.Terminal(rs.status) {
		if s.active == rs {
			s.active = nil
		}
		h.mu.Lock()
		delete(h.runs, rs.id)
		h.mu.Unlock()
		h.log.Info("run finished", "session_id", rs.sessionID, "run_id", rs.id, "status", rs.status,
			"events", rs.events, "duration_ms", time.Since(rs.started).Milliseconds())
	}
}

// Subscribe adds sub to a session. With afterSeq, stored events after it are delivered first (or replay_gap);
// without it, a pending approval request of the active run is (see pendingApproval). The active run's status follows.
func (h *Hub) Subscribe(ctx context.Context, sub Subscriber, sessionID string, afterSeq *int64) error {
	s := h.lockSession(sessionID)
	defer h.unlockSession(s)
	if afterSeq != nil {
		evs, err := h.store.EventsAfter(ctx, sessionID, *afterSeq, h.opts.MaxReplay)
		if errors.Is(err, store.ErrReplayGap) {
			return protocol.Errorf(protocol.CodeReplayGap, "events after seq %d are no longer stored; reload the history", *afterSeq)
		} else if err != nil {
			h.log.Error("replay failed", "session_id", sessionID, "err", err.Error())
			return protocol.Errorf(protocol.CodeInternal, "internal error")
		}
		for _, e := range evs {
			sub.Deliver(eventMsg(e))
		}
	}
	s.subs[sub] = struct{}{}
	if s.active != nil {
		if afterSeq == nil {
			h.pendingApproval(ctx, sub, s.active)
		}
		sub.Deliver(runMsg(s.active))
	}
	return nil
}

// pendingApproval delivers the approval.requested event of a run waiting for approval, again, with its original seq.
// A subscriber without after_seq has none of the session's events: it would see the wait but not the request it
// can answer. With after_seq the replay already carries it, or the client saw it live. Session lock held.
func (h *Hub) pendingApproval(ctx context.Context, sub Subscriber, rs *runState) {
	if rs.status != protocol.StatusWaitingForApproval {
		return
	}
	e, err := h.store.LastEvent(ctx, rs.sessionID, rs.id, protocol.KindApprovalRequested)
	switch {
	case err == nil:
		sub.Deliver(eventMsg(e))
	case !errors.Is(err, store.ErrNotFound):
		h.log.Error("reading the pending approval failed", "session_id", rs.sessionID, "run_id", rs.id, "err", err.Error())
	}
}

// Unsubscribe removes sub from a session.
func (h *Hub) Unsubscribe(sub Subscriber, sessionID string) {
	s := h.lockSession(sessionID)
	delete(s.subs, sub)
	h.unlockSession(s)
}

// SessionSource returns a session's Hermes source (api_server, telegram, cli...), cached.
func (h *Hub) SessionSource(ctx context.Context, id string) (string, error) {
	h.mu.Lock()
	src, ok := h.sources[id]
	h.mu.Unlock()
	if ok {
		return src, nil
	}
	sess, err := h.hermes.GetSession(ctx, id)
	if err != nil {
		return "", err
	}
	h.RememberSource(id, sess.Source)
	return sess.Source, nil
}

// RememberSource caches a session's source (sources never change).
func (h *Hub) RememberSource(id, source string) {
	if !protocol.ValidID(id) || len(source) > 64 {
		return
	}
	h.mu.Lock()
	if len(h.sources) >= 10000 {
		clear(h.sources)
	}
	h.sources[id] = source
	h.mu.Unlock()
}

// ForgetSource drops a deleted session from the cache.
func (h *Hub) ForgetSource(id string) {
	h.mu.Lock()
	delete(h.sources, id)
	h.mu.Unlock()
}

// SourceAPIServer is the only source the phone may write to.
const SourceAPIServer = "api_server"

// Prompt starts a run for clientMsgID, or acknowledges the run it already started. onAck runs before any of the
// run's events are delivered.
func (h *Hub) Prompt(ctx context.Context, deviceID int64, clientMsgID, sessionID, text string, onAck func(runID string, replayed bool)) error {
	if r, err := h.store.RunByClientMsgID(ctx, clientMsgID); err == nil {
		onAck(r.RunID, true)
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return protocol.Errorf(protocol.CodeInternal, "internal error")
	}
	h.mu.Lock()
	if p := h.inflight[clientMsgID]; p != nil {
		h.mu.Unlock()
		select {
		case <-p.done:
		case <-ctx.Done():
			return protocol.Errorf(protocol.CodeInternal, "cancelled")
		}
		if p.err != nil {
			return p.err
		}
		onAck(p.runID, true)
		return nil
	}
	p := &pending{done: make(chan struct{})}
	h.inflight[clientMsgID] = p
	h.mu.Unlock()

	p.runID, p.err = h.startRun(ctx, deviceID, clientMsgID, sessionID, text, onAck)
	h.mu.Lock()
	delete(h.inflight, clientMsgID)
	h.mu.Unlock()
	close(p.done)
	return p.err
}

func (h *Hub) startRun(ctx context.Context, deviceID int64, clientMsgID, sessionID, text string, onAck func(string, bool)) (string, error) {
	src, err := h.SessionSource(ctx, sessionID)
	if err != nil {
		return "", hermes.ProtocolError(err)
	}
	if src != SourceAPIServer {
		return "", protocol.Errorf(protocol.CodeReadOnly, "this session belongs to %s and is read-only here", trunc(src, 32))
	}
	s := h.lockSession(sessionID)
	if s.active != nil || s.starting {
		h.unlockSession(s)
		return "", protocol.Errorf(protocol.CodeBusy, "a run is already active in this session")
	}
	s.starting = true
	s.mu.Unlock()

	// Detached from the socket: if the phone drops now, Hermes may still start the run, and the resend with the
	// same client_msg_id finds it through the Idempotency-Key.
	created, err := h.hermes.CreateRun(context.WithoutCancel(ctx), sessionID, text, clientMsgID)

	s.mu.Lock()
	s.starting = false
	if err != nil {
		h.unlockSession(s)
		h.log.Warn("starting run failed", "session_id", sessionID, "hermes_status", hermes.StatusOf(err))
		return "", hermes.ProtocolError(err)
	}
	defer h.unlockSession(s)
	if err := h.store.InsertRun(context.Background(), store.Run{RunID: created.RunID, SessionID: sessionID,
		ClientMsgID: clientMsgID, DeviceID: deviceID, Status: protocol.StatusRunning}); err != nil {
		h.log.Error("storing run failed", "run_id", created.RunID, "err", err.Error())
		return "", protocol.Errorf(protocol.CodeInternal, "internal error")
	}
	rs := &runState{id: created.RunID, sessionID: sessionID, status: protocol.StatusRunning, hermesSeq: -1, started: time.Now()}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		onAck(rs.id, created.Replayed) // the next start re-attaches it
		return rs.id, nil
	}
	h.runs[rs.id] = rs
	h.wg.Add(1)
	h.mu.Unlock()
	s.active = rs
	onAck(rs.id, created.Replayed)
	msg := runMsg(rs)
	for sub := range s.subs {
		sub.Deliver(msg)
	}
	h.log.Info("run started", "session_id", sessionID, "run_id", rs.id, "device_id", deviceID, "replayed", created.Replayed)
	go h.track(rs, false)
	return rs.id, nil
}

// controllable finds an active run that sub may act on: only subscribers of the run's session.
func (h *Hub) controllable(sub Subscriber, runID string) (*runState, error) {
	h.mu.Lock()
	rs := h.runs[runID]
	h.mu.Unlock()
	if rs == nil {
		return nil, protocol.Errorf(protocol.CodeNotFound, "no active run with that id in your subscribed sessions")
	}
	s := h.lockSession(rs.sessionID)
	_, ok := s.subs[sub]
	h.unlockSession(s)
	if !ok {
		return nil, protocol.Errorf(protocol.CodeNotFound, "no active run with that id in your subscribed sessions")
	}
	return rs, nil
}

func (h *Hub) control(rs *runState, what string, err error) error {
	if err != nil {
		h.log.Warn(what+" failed", "session_id", rs.sessionID, "run_id", rs.id, "hermes_status", hermes.StatusOf(err))
		return hermes.ProtocolError(err)
	}
	h.log.Info(what, "session_id", rs.sessionID, "run_id", rs.id)
	return nil
}

// Stop asks Hermes to interrupt a run.
func (h *Hub) Stop(ctx context.Context, sub Subscriber, runID string) error {
	rs, err := h.controllable(sub, runID)
	if err != nil {
		return err
	}
	return h.control(rs, "stop", h.hermes.Stop(context.WithoutCancel(ctx), runID))
}

// Steer sends extra guidance to a running run; on success a steer.queued event follows.
func (h *Hub) Steer(ctx context.Context, sub Subscriber, runID, text string) error {
	rs, err := h.controllable(sub, runID)
	if err != nil {
		return err
	}
	if err := h.control(rs, "steer", h.hermes.Steer(context.WithoutCancel(ctx), runID, text)); err != nil {
		return err
	}
	h.emit(rs, []store.NewEvent{{Kind: protocol.KindSteerQueued, Data: []byte("{}"), TS: time.Now().UnixMilli()}}, store.RunUpdate{HermesSeq: -1})
	return nil
}

// Approve answers a pending approval with once, session or deny.
func (h *Hub) Approve(ctx context.Context, sub Subscriber, runID, requestID, choice string) error {
	switch choice {
	case "once", "session", "deny":
	default:
		return protocol.Errorf(protocol.CodeBadRequest, "choice must be once, session or deny")
	}
	rs, err := h.controllable(sub, runID)
	if err != nil {
		return err
	}
	err = h.hermes.Approve(context.WithoutCancel(ctx), runID, requestID, choice)
	if err == nil {
		h.log.Info("approval answered", "session_id", rs.sessionID, "run_id", rs.id, "choice", choice)
		return nil
	}
	return h.control(rs, "approval", err)
}

// ActiveRun returns the id of a session's active run, if any.
func (h *Hub) ActiveRun(sessionID string) string {
	s := h.lockSession(sessionID)
	defer h.unlockSession(s)
	if s.active != nil {
		return s.active.id
	}
	return ""
}

// isNotFound reports a Hermes 404.
func isNotFound(err error) bool { return hermes.StatusOf(err) == http.StatusNotFound }
