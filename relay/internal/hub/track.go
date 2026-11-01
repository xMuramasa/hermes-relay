package hub

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// track follows one run to its end: it reads the event stream and, whenever the stream ends without a terminal
// frame, polls GET /v1/runs/{id} with backoff, re-attaching while the run is still going. Hermes seqs already
// handled are skipped, so re-reading a stream never duplicates events. After a relay restart (resumed) the run
// starts with a reconnected event.
func (h *Hub) track(rs *runState, resumed bool) {
	defer h.wg.Done()
	ctx := h.ctx
	log := h.log.With("session_id", rs.sessionID, "run_id", rs.id)
	var lostAt time.Time // zero while the stream is believed healthy
	backoff := h.opts.PollMin
	wait := func(err error) bool {
		d := max(backoff, hermes.RetryAfterOf(err))
		backoff = min(backoff*2, h.opts.PollMax)
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	if resumed {
		h.reconnected(rs, "relay_restart")
		lostAt = time.Now()
	}
	for ctx.Err() == nil {
		if !lostAt.IsZero() {
			st, err := h.hermes.GetRun(ctx, rs.id)
			switch {
			case ctx.Err() != nil:
				return
			case err == nil && protocol.Terminal(st.Status):
				if h.finishFromStatus(rs, st) == nil {
					return
				}
				if !wait(nil) { // storing failed: poll again
					return
				}
				continue
			case err == nil:
				h.syncStatus(rs, st)
			case isNotFound(err):
				log.Warn("Hermes no longer knows the run")
				h.fail(rs, "Hermes no longer knows this run")
				return
			default:
				if time.Since(lostAt) > h.opts.GiveUp {
					log.Error("giving up on run", "hermes_status", hermes.StatusOf(err))
					h.fail(rs, "lost contact with Hermes")
					return
				}
				if !wait(err) {
					return
				}
				continue
			}
		}
		fresh := 0
		var storeErr error
		stopped, err := h.hermes.Events(ctx, rs.id, func(f hermes.Frame) bool {
			terminal, isNew, err := h.handleFrame(rs, f)
			if err != nil {
				storeErr = err
				return true
			}
			if isNew {
				fresh++
			}
			return terminal
		})
		if storeErr != nil {
			// The frame was not stored and hermesSeq did not move: treat the stream as lost, so GET /v1/runs/{id}
			// (or the next stream read) delivers it again.
			stopped, err = false, storeErr
		}
		if stopped || ctx.Err() != nil {
			return
		}
		log.Warn("event stream lost", "reason", streamErr(err), "hermes_status", hermes.StatusOf(err), "new_frames", fresh)
		if lostAt.IsZero() || fresh > 0 {
			lostAt, backoff = time.Now(), h.opts.PollMin
			h.reconnected(rs, "stream_lost")
			continue
		}
		if time.Since(lostAt) > h.opts.GiveUp {
			log.Error("giving up on run")
			h.fail(rs, "lost contact with Hermes")
			return
		}
		if !wait(err) {
			return
		}
	}
}

func streamErr(err error) string {
	switch {
	case err == nil:
		return "ended"
	case errors.Is(err, hermes.ErrStreamDead):
		return "silent"
	case errors.Is(err, hermes.ErrUnavailable):
		return "unavailable"
	default:
		return "error"
	}
}

// handleFrame maps and stores one frame. isNew is false for frames of another run or already handled. On a store
// error the frame counts as not handled.
func (h *Hub) handleFrame(rs *runState, f hermes.Frame) (terminal, isNew bool, err error) {
	if f.RunID != "" && f.RunID != rs.id {
		return false, false, nil
	}
	if f.Seq >= 0 && f.Seq <= rs.hermesSeq {
		return false, false, nil
	}
	m := mapFrame(f, time.Now())
	if err := h.emit(rs, m.events, store.RunUpdate{HermesSeq: f.Seq, Status: m.status}); err != nil {
		return false, false, err
	}
	if m.status == protocol.StatusWaitingForApproval {
		var rid string
		_ = json.Unmarshal(f.Fields["request_id"], &rid)
		rs.lastApproval = rid
	}
	if f.Seq >= 0 {
		rs.hermesSeq = f.Seq
	}
	return m.terminal, true, nil
}

func (h *Hub) reconnected(rs *runState, reason string) {
	data, _ := json.Marshal(map[string]string{"reason": reason})
	_ = h.emit(rs, []store.NewEvent{{Kind: protocol.KindReconnected, Data: data, TS: time.Now().UnixMilli()}}, store.RunUpdate{HermesSeq: -1})
}

// fail ends the run as failed. If that can't be stored the session is still freed; the database keeps the run in
// progress, so the next start asks Hermes about it again.
func (h *Hub) fail(rs *runState, msg string) {
	data, _ := json.Marshal(map[string]string{"error": msg})
	if h.emit(rs, []store.NewEvent{{Kind: protocol.KindRunFailed, Data: data, TS: time.Now().UnixMilli()}},
		store.RunUpdate{HermesSeq: -1, Status: protocol.StatusFailed}) != nil {
		s := h.lockSession(rs.sessionID)
		h.setStatus(s, rs, protocol.StatusFailed)
		h.unlockSession(s)
	}
}

// finishFromStatus ends a run from GET /v1/runs/{id}: the body has the same fields as the terminal frame.
func (h *Hub) finishFromStatus(rs *runState, st hermes.RunStatus) error {
	m := mapFrame(hermes.Frame{Event: "run." + st.Status, Seq: -1, Fields: st.Fields}, time.Now())
	return h.emit(rs, m.events, store.RunUpdate{HermesSeq: -1, Status: m.status})
}

// syncStatus catches up on what a lost stream may have hidden: a pending approval, or its resolution.
func (h *Hub) syncStatus(rs *runState, st hermes.RunStatus) {
	switch st.Status {
	case protocol.StatusWaitingForApproval:
		var fields map[string]json.RawMessage
		if json.Unmarshal(st.Fields["approval"], &fields) != nil || st.Approval == nil || st.Approval.RequestID == rs.lastApproval {
			return
		}
		m := mapFrame(hermes.Frame{Event: "approval.request", Seq: -1, Fields: fields}, time.Now())
		if m.status != "" && h.emit(rs, m.events, store.RunUpdate{HermesSeq: -1, Status: m.status}) == nil {
			rs.lastApproval = st.Approval.RequestID
		}
	case protocol.StatusRunning:
		_ = h.emit(rs, nil, store.RunUpdate{HermesSeq: -1, Status: protocol.StatusRunning})
	}
}
