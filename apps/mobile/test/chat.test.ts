import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { parseMessage } from "../src/lib/api";
import { chatReducer, initialChat, isBusy, type Action, type ChatState } from "../src/lib/chat";
import type { EventData, EventKind, ServerMessage } from "../src/lib/protocol";

const SID = "api_1792801001_b6235dd4";
const RUN = "run_43f98388dbc54cd1ab911657e79cf8af";
const CID = "6f1c2b8e-1d2a-4c55-9a0e-1b2c3d4e5f60";

let seq = 0;
function ev<K extends EventKind>(kind: K, data: EventData[K], opts: { seq?: number; session?: string } = {}): Action {
  const msg = { type: "event", session_id: opts.session ?? SID, run_id: RUN, seq: opts.seq ?? ++seq, kind, data, ts: 1792801004583 } as ServerMessage;
  return { type: "server", msg };
}
const srv = (msg: ServerMessage): Action => ({ type: "server", msg });
const run = (...actions: Action[]): ChatState => actions.reduce(chatReducer, initialChat(SID));
const kinds = (s: ChatState) => s.items.map((i) => i.kind);

const sent = (): Action[] => [
  { type: "send", clientMsgId: CID, text: "what year is it?" },
  srv({ type: "ack", client_msg_id: CID, run_id: RUN, replayed: false }),
];

describe("chat reducer", () => {
  test("plain answer: pending → acked → running → answer → done", () => {
    let s = run({ type: "send", clientMsgId: CID, text: "hi" });
    expect(s.items[0]).toMatchObject({ kind: "user", status: "pending" });
    expect(isBusy(s)).toBe(true);
    s = chatReducer(s, srv({ type: "ack", client_msg_id: CID, run_id: RUN, replayed: false }));
    expect(s.items[0]).toMatchObject({ status: "sent" });
    expect(s.run).toEqual({ id: RUN, status: "running" });
    s = chatReducer(s, ev("answer", { text: "Hello." }));
    s = chatReducer(s, ev("run.completed", {}));
    expect(kinds(s)).toEqual(["user", "assistant"]);
    expect(s.items[1]).toMatchObject({ text: "Hello." });
    expect(s.run).toBeNull();
    expect(isBusy(s)).toBe(false);
  });

  test("tool run: started shows the current tool, completed fills the same row", () => {
    let s = run(...sent(), ev("tool.started", { tool: "terminal", preview: "date +%Y" }));
    expect(s.currentTool).toBe("terminal");
    s = chatReducer(s, ev("tool.completed", { tool: "terminal", error: false, duration: 1.3, preview: '{"output": "2026"}' }));
    expect(s.currentTool).toBeNull();
    s = run(...sent(), ev("tool.started", { tool: "terminal", preview: "date +%Y" }), ev("tool.completed", { tool: "terminal", error: true, duration: 1.3 }), ev("answer", { text: "2026" }), ev("run.completed", {}));
    expect(kinds(s)).toEqual(["user", "tool", "assistant"]);
    expect(s.items[1]).toMatchObject({ tool: "terminal", args: "date +%Y", duration: 1.3, error: true, done: true });
  });

  test("subagent card: started then completed", () => {
    const s = run(
      ...sent(),
      ev("subagent.started", { subagent_id: "sa_1", goal: "count files", task_index: 0, task_count: 1 }),
      ev("subagent.completed", { subagent_id: "sa_1", status: "completed", summary: "12 files", duration: 4.2 }),
    );
    expect(s.items[1]).toMatchObject({ kind: "subagent", goal: "count files", status: "completed", summary: "12 files", duration: 4.2, done: true });
    expect(s.run).not.toBeNull();
  });

  test("failed run ends the turn with an error line", () => {
    const s = run(...sent(), ev("run.failed", { error: "provider error" }));
    expect(s.items.at(-1)).toMatchObject({ kind: "status", tone: "error", text: "Run failed: provider error" });
    expect(s.run).toBeNull();
  });

  test("busy error: dismissible notice on the pending message, then dismiss removes it", () => {
    let s = run({ type: "send", clientMsgId: CID, text: "again" }, srv({ type: "error", code: "busy", message: "a run is active", ref: CID }));
    expect(s.items[0]).toMatchObject({ status: "failed", notice: { code: "busy" } });
    expect(isBusy(s)).toBe(false);
    s = chatReducer(s, { type: "dismiss", key: s.items[0]!.key });
    expect(s.items).toEqual([]);
  });

  test("errors for other refs, or without one, change nothing", () => {
    const s0 = run({ type: "send", clientMsgId: CID, text: "x" });
    expect(chatReducer(s0, srv({ type: "error", code: "busy", message: "", ref: "other" }))).toBe(s0);
    expect(chatReducer(s0, srv({ type: "error", code: "internal", message: "" }))).toBe(s0);
  });

  test("duplicate seq is applied once", () => {
    const a = ev("answer", { text: "once" }, { seq: 5 });
    const s = run(...sent(), a, a, ev("tool.started", { tool: "t" }, { seq: 4 }));
    expect(kinds(s)).toEqual(["user", "assistant"]);
    expect(s.lastSeq).toBe(5);
  });

  test("unknown kind is ignored but its seq is counted", () => {
    const s0 = run(...sent());
    const s = chatReducer(s0, ev("unknown", { event: "message.interim", data: { text: "x" } }, { seq: 100 }));
    expect(s.items).toEqual(s0.items);
    expect(s.lastSeq).toBe(100);
  });

  test("events for another session are ignored", () => {
    const s0 = run(...sent());
    expect(chatReducer(s0, ev("answer", { text: "elsewhere" }, { session: "api_other" }))).toBe(s0);
  });

  test("run status messages set and clear the active run", () => {
    let s = run(srv({ type: "run", run_id: RUN, session_id: SID, status: "waiting_for_approval" }));
    expect(s.run).toEqual({ id: RUN, status: "waiting_for_approval" });
    s = chatReducer(s, srv({ type: "run", run_id: RUN, session_id: SID, status: "completed" }));
    expect(s.run).toBeNull();
  });

  test("history from a real recording, then live events merge after it", () => {
    const lines = readFileSync(join(import.meta.dir, "../../../probe/fixtures/tool.jsonl"), "utf8").split("\n");
    const body = JSON.parse(lines.find((l) => l.includes('"kind": "session.messages"'))!).body;
    const messages = body.data.map(parseMessage);

    // A live prompt sent before the history arrived must survive the merge.
    let s = run({ type: "send", clientMsgId: CID, text: "and next year?" });
    s = chatReducer(s, { type: "history", messages });
    expect(s.historyLoaded).toBe(true);
    expect(kinds(s)).toEqual(["user", "tool", "assistant", "user"]);
    expect(s.items[1]).toMatchObject({ tool: "terminal", args: '{"command":"date +%Y"}', done: true });
    expect((s.items[1] as { result: string }).result).toContain("2026");
    expect(s.items[2]).toMatchObject({ text: "The terminal printed 2026." });

    s = chatReducer(s, srv({ type: "ack", client_msg_id: CID, run_id: RUN, replayed: false }));
    s = chatReducer(s, ev("answer", { text: "2027." }));
    s = chatReducer(s, ev("run.completed", {}));
    expect(kinds(s)).toEqual(["user", "tool", "assistant", "user", "assistant"]);
    expect(s.items.at(-2)).toMatchObject({ status: "sent" });
  });
});
