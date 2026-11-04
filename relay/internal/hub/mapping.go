package hub

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// Size caps for event data (in characters, like the schema's maxLength).
const (
	capShort      = 128
	capPreview    = 4000
	capLong       = 16000
	capAnswer     = 262144
	capUnknownRaw = 8 << 10
)

// mapped is what one Hermes frame turns into.
type mapped struct {
	events   []store.NewEvent
	status   string // new run status, or ""
	terminal bool
}

// Hermes events dropped on purpose: reasoning.available repeats the final answer, message.delta is text streaming
// (off), run.steered duplicates the relay's own steer.queued.
var dropped = map[string]bool{"reasoning.available": true, "message.delta": true, "run.steered": true}

// mapFrame turns one Hermes run event into relay events (SPEC "event.kind"), keeping only what the app needs.
func mapFrame(f hermes.Frame, now time.Time) mapped {
	ts := now.UnixMilli()
	if f.Timestamp > 1e9 && f.Timestamp < 1e11 {
		ts = int64(f.Timestamp * 1000)
	}
	fl := fields(f.Fields)
	one := func(kind string, data map[string]any) []store.NewEvent {
		return []store.NewEvent{{Kind: kind, Data: mustJSON(data), TS: ts}}
	}
	switch f.Event {
	case "tool.started":
		d := map[string]any{"tool": fl.short("tool", "tool")}
		fl.optStr(d, "preview", "preview", capPreview)
		return mapped{events: one(protocol.KindToolStarted, d)}
	case "tool.completed":
		d := map[string]any{"tool": fl.short("tool", "tool"), "error": fl.boolean("error")}
		fl.optDuration(d, "duration", "duration")
		fl.optStr(d, "preview", "preview", capPreview)
		return mapped{events: one(protocol.KindToolCompleted, d)}
	case "subagent.start", "subagent.started":
		id, ok := fl.str("subagent_id")
		if !ok || id == "" {
			break
		}
		d := map[string]any{"subagent_id": trunc(id, capShort)}
		fl.optStr(d, "goal", "goal", capPreview)
		fl.optStr(d, "model", "model", capShort)
		fl.optStr(d, "child_session_id", "child_session_id", capShort)
		fl.optCount(d, "task_index", "task_index")
		fl.optCount(d, "task_count", "task_count")
		return mapped{events: one(protocol.KindSubagentStarted, d)}
	case "subagent.complete", "subagent.completed":
		id, ok := fl.str("subagent_id")
		if !ok || id == "" {
			break
		}
		d := map[string]any{"subagent_id": trunc(id, capShort)}
		fl.optStr(d, "status", "status", capShort)
		fl.optStr(d, "summary", "summary", capLong)
		if _, ok := fl.num("duration_seconds"); ok {
			fl.optDuration(d, "duration", "duration_seconds")
		} else {
			fl.optDuration(d, "duration", "duration")
		}
		return mapped{events: one(protocol.KindSubagentCompleted, d)}
	case "approval.request":
		rid, _ := fl.str("request_id")
		if !protocol.ValidID(rid) {
			break
		}
		cmd, _ := fl.str("command")
		d := map[string]any{"request_id": rid, "command": trunc(cmd, capLong), "choices": approvalChoices(fl)}
		fl.optStr(d, "description", "description", capPreview)
		return mapped{events: one(protocol.KindApprovalRequested, d), status: protocol.StatusWaitingForApproval}
	case "approval.responded":
		rid, _ := fl.str("request_id")
		if !protocol.ValidID(rid) {
			break
		}
		choice, _ := fl.str("choice")
		if !choiceRe.MatchString(choice) {
			choice = "unknown"
		}
		return mapped{events: one(protocol.KindApprovalResolved, map[string]any{"request_id": rid, "choice": choice}),
			status: protocol.StatusRunning}
	case "run.completed":
		out, _ := fl.str("output")
		d := map[string]any{"text": trunc(out, capAnswer)}
		if utf8.RuneCountInString(out) > capAnswer {
			d["truncated"] = true
		}
		evs := one(protocol.KindAnswer, d)
		evs = append(evs, store.NewEvent{Kind: protocol.KindRunCompleted, Data: []byte("{}"), TS: ts})
		return mapped{events: evs, status: protocol.StatusCompleted, terminal: true}
	case "run.failed", "run.interrupted": // interrupted: the gateway restarted before the run settled
		d := map[string]any{}
		if msg := fl.errorText(); msg != "" {
			d["error"] = trunc(msg, capPreview)
		}
		return mapped{events: one(protocol.KindRunFailed, d), status: protocol.StatusFailed, terminal: true}
	case "run.cancelled":
		d := map[string]any{}
		fl.optStr(d, "reason", "turn_exit_reason", capShort)
		return mapped{events: one(protocol.KindRunCancelled, d), status: protocol.StatusCancelled, terminal: true}
	default:
		if dropped[f.Event] {
			return mapped{}
		}
	}
	return mapped{events: one(protocol.KindUnknown, unknownData(f))}
}

var (
	choiceRe    = regexp.MustCompile(`^[a-z_]{1,32}$`)
	eventNameRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
)

// approvalChoices offers once/session/deny in that order, as far as Hermes allows them; never always.
func approvalChoices(fl fieldMap) []string {
	offered := map[string]bool{"once": true, "session": true, "deny": true}
	var list []string
	if raw, ok := fl["choices"]; ok && json.Unmarshal(raw, &list) == nil && len(list) > 0 {
		offered = map[string]bool{}
		for _, c := range list {
			offered[c] = true
		}
	}
	if raw, ok := fl["allow_session"]; ok {
		var allow bool
		if json.Unmarshal(raw, &allow) == nil && !allow {
			offered["session"] = false
		}
	}
	var out []string
	for _, c := range []string{"once", "session", "deny"} {
		if offered[c] {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		out = []string{"deny"}
	}
	return out
}

func unknownData(f hermes.Frame) map[string]any {
	name := f.Event
	if !eventNameRe.MatchString(name) {
		name = "invalid"
	}
	rest := map[string]json.RawMessage{}
	for k, v := range f.Fields {
		switch k {
		case "event", "run_id", "seq", "timestamp":
		default:
			rest[k] = v
		}
	}
	raw := mustJSON(rest)
	if len(raw) > capUnknownRaw {
		return map[string]any{"event": name, "data": map[string]any{}, "truncated": true}
	}
	return map[string]any{"event": name, "data": json.RawMessage(raw)}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// trunc cuts s to n characters (code points) and replaces invalid UTF-8.
func trunc(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

type fieldMap map[string]json.RawMessage

func fields(m map[string]json.RawMessage) fieldMap { return fieldMap(m) }

func (f fieldMap) str(k string) (string, bool) {
	var s string
	if raw, ok := f[k]; ok && json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return "", false
}

func (f fieldMap) short(k, def string) string {
	if s, ok := f.str(k); ok && s != "" {
		return trunc(s, capShort)
	}
	return def
}

func (f fieldMap) optStr(d map[string]any, out, k string, n int) {
	if s, ok := f.str(k); ok && s != "" {
		d[out] = trunc(s, n)
	}
}

func (f fieldMap) num(k string) (float64, bool) {
	var v float64
	if raw, ok := f[k]; ok && json.Unmarshal(raw, &v) == nil {
		return v, true
	}
	return 0, false
}

func (f fieldMap) boolean(k string) bool {
	var v bool
	if raw, ok := f[k]; ok {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}

func (f fieldMap) optDuration(d map[string]any, out, k string) {
	if v, ok := f.num(k); ok && v >= 0 && v < 1e9 {
		d[out] = v
	}
}

func (f fieldMap) optCount(d map[string]any, out, k string) {
	if v, ok := f.num(k); ok && v >= 0 && v < 1e6 && v == float64(int64(v)) {
		d[out] = int64(v)
	}
}

// errorText reads Hermes' error as a string or {message}.
func (f fieldMap) errorText() string {
	if s, ok := f.str("error"); ok {
		return s
	}
	var obj struct {
		Message string `json:"message"`
	}
	if raw, ok := f["error"]; ok && json.Unmarshal(raw, &obj) == nil {
		return obj.Message
	}
	return ""
}
