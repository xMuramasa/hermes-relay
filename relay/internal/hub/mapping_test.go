package hub

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/fakehermes"
	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
)

var v = protocol.MustValidator()

// asMessage wraps a mapped event as the relay would send it and validates it.
func checkEvent(t *testing.T, kind string, data []byte) {
	t.Helper()
	msg, _ := json.Marshal(protocol.Event{Type: "event", SessionID: "api_1", RunID: "run_1", Seq: 1, Kind: kind, Data: data, TS: 1})
	if err := v.ValidateDetail(msg); err != nil {
		t.Fatalf("mapped %s event fails the schema: %v\n%s", kind, err, msg)
	}
}

func frameFrom(t *testing.T, m map[string]any) hermes.Frame {
	t.Helper()
	b, _ := json.Marshal(m)
	f, err := hermes.ParseFrame(b)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mapAll(t *testing.T, frames []map[string]any) (kinds []string, statuses []string, terminal bool) {
	for _, fr := range frames {
		m := mapFrame(frameFrom(t, fr), time.Now())
		for _, e := range m.events {
			checkEvent(t, e.Kind, e.Data)
			kinds = append(kinds, e.Kind)
		}
		if m.status != "" {
			statuses = append(statuses, m.status)
		}
		terminal = m.terminal
	}
	return
}

func TestMapFixtures(t *testing.T) {
	want := map[string]string{
		"plain":    "answer run.completed",
		"tool":     "tool.started tool.completed answer run.completed",
		"subagent": "tool.started subagent.started tool.completed subagent.completed answer run.completed",
		"approval": "tool.started approval.requested approval.resolved tool.completed answer run.completed",
		"stop":     "run.cancelled",
	}
	for name, w := range want {
		fx, err := fakehermes.LoadFixture(filepath.Join("..", "..", "..", "probe", "fixtures", name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		kinds, _, terminal := mapAll(t, fx.Frames)
		if got := strings.Join(kinds, " "); got != w || !terminal {
			t.Errorf("%s: %q terminal=%v, want %q", name, got, terminal, w)
		}
	}
}

func TestMapChatStreamFixtureIsUnknown(t *testing.T) {
	// The chat stream (not used by the relay) has other event names: they must map to unknown or terminal
	// events without breaking the schema.
	fx, err := fakehermes.LoadFixture(filepath.Join("..", "..", "..", "probe", "fixtures", "chat_stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fx.Frames) != 0 {
		t.Fatalf("chat_stream has no sse lines in the run format, got %d", len(fx.Frames))
	}
	kinds, _, _ := mapAll(t, []map[string]any{
		{"event": "run.started", "seq": 1}, {"event": "message.started", "seq": 2, "message": map[string]any{"id": "m"}},
		{"event": "tool.progress", "seq": 3, "tool_name": "_thinking", "delta": "x"},
	})
	if strings.Join(kinds, " ") != "unknown unknown unknown" {
		t.Fatal(kinds)
	}
}

func TestMapApprovalChoices(t *testing.T) {
	base := map[string]any{"event": "approval.request", "request_id": "a990e19b6d5049798852410f79f88255", "command": "rm -rf x",
		"choices": []string{"once", "session", "always", "deny"}, "allow_session": true}
	data := func(m map[string]any) map[string]any {
		r := mapFrame(frameFrom(t, m), time.Now())
		checkEvent(t, r.events[0].Kind, r.events[0].Data)
		var d map[string]any
		_ = json.Unmarshal(r.events[0].Data, &d)
		return d
	}
	if d := data(base); fmt.Sprint(d["choices"]) != "[once session deny]" {
		t.Fatalf("choices %v", d["choices"])
	}
	noSession := map[string]any{}
	for k, val := range base {
		noSession[k] = val
	}
	noSession["allow_session"] = false
	if d := data(noSession); fmt.Sprint(d["choices"]) != "[once deny]" {
		t.Fatalf("choices %v", d["choices"])
	}
	onlyAlways := map[string]any{"event": "approval.request", "request_id": "r1", "command": "x", "choices": []string{"always"}}
	if d := data(onlyAlways); fmt.Sprint(d["choices"]) != "[deny]" {
		t.Fatalf("choices %v", d["choices"])
	}
	badID := map[string]any{"event": "approval.request", "request_id": "../x", "command": "x"}
	if r := mapFrame(frameFrom(t, badID), time.Now()); r.events[0].Kind != protocol.KindUnknown {
		t.Fatalf("bad request id mapped to %s", r.events[0].Kind)
	}
}

func TestMapCapsAndUnknown(t *testing.T) {
	long := strings.Repeat("é", capPreview+10)
	r := mapFrame(frameFrom(t, map[string]any{"event": "tool.started", "tool": strings.Repeat("t", 500), "preview": long}), time.Now())
	checkEvent(t, r.events[0].Kind, r.events[0].Data)

	huge := strings.Repeat("a", capAnswer+5)
	r = mapFrame(frameFrom(t, map[string]any{"event": "run.completed", "output": huge, "usage": map[string]any{"input_tokens": 1}}), time.Now())
	checkEvent(t, r.events[0].Kind, r.events[0].Data)
	if !strings.Contains(string(r.events[0].Data), `"truncated":true`) || string(r.events[1].Data) != "{}" {
		t.Fatal("answer not truncated or run.completed not empty")
	}

	r = mapFrame(frameFrom(t, map[string]any{"event": "brand.new", "seq": 3, "x": 1}), time.Now())
	checkEvent(t, r.events[0].Kind, r.events[0].Data)
	if string(r.events[0].Data) != `{"data":{"x":1},"event":"brand.new"}` {
		t.Fatalf("unknown data %s", r.events[0].Data)
	}
	r = mapFrame(frameFrom(t, map[string]any{"event": "brand.big", "blob": strings.Repeat("b", 9000)}), time.Now())
	checkEvent(t, r.events[0].Kind, r.events[0].Data)
	if string(r.events[0].Data) != `{"data":{},"event":"brand.big","truncated":true}` {
		t.Fatalf("big unknown data %s", r.events[0].Data)
	}
	r = mapFrame(frameFrom(t, map[string]any{"event": "bad name<script>"}), time.Now())
	checkEvent(t, r.events[0].Kind, r.events[0].Data)

	for _, ev := range []string{"reasoning.available", "message.delta", "run.steered"} {
		if r := mapFrame(frameFrom(t, map[string]any{"event": ev, "text": "x"}), time.Now()); len(r.events) != 0 {
			t.Errorf("%s should be dropped", ev)
		}
	}
	r = mapFrame(frameFrom(t, map[string]any{"event": "run.failed", "error": map[string]any{"message": "provider down"}}), time.Now())
	checkEvent(t, r.events[0].Kind, r.events[0].Data)
	if string(r.events[0].Data) != `{"error":"provider down"}` || r.status != protocol.StatusFailed || !r.terminal {
		t.Fatalf("run.failed %s", r.events[0].Data)
	}
}

func TestMapTimestamp(t *testing.T) {
	r := mapFrame(frameFrom(t, map[string]any{"event": "run.cancelled", "timestamp": 1792801164.1581926}), time.Now())
	if r.events[0].TS != 1792801164158 {
		t.Fatalf("ts %d", r.events[0].TS)
	}
	now := time.Now()
	r = mapFrame(frameFrom(t, map[string]any{"event": "run.cancelled", "timestamp": "nope"}), now)
	if r.events[0].TS != now.UnixMilli() {
		t.Fatalf("ts %d", r.events[0].TS)
	}
}
