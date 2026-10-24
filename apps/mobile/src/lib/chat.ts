// Chat state for one session: REST history merged with live relay messages. Pure; no React.
import type { HistoryMessage } from "./api";
import type { ErrorCode, EventMessage, RunStatus, ServerMessage } from "./protocol";

export type Notice = { code: ErrorCode; message: string };

export type Item =
  | {
      kind: "user";
      key: string;
      text: string;
      status: "pending" | "sent" | "failed";
      clientMsgId?: string;
      notice?: Notice;
    }
  | { kind: "assistant"; key: string; text: string; truncated?: boolean }
  | {
      kind: "tool";
      key: string;
      tool: string;
      args?: string;
      result?: string;
      duration?: number;
      error: boolean;
      done: boolean;
      runId?: string;
      callId?: string;
    }
  | { kind: "subagent"; key: string; id: string; goal?: string; status?: string; summary?: string; duration?: number; done: boolean }
  | { kind: "status"; key: string; text: string; tone: "error" | "muted" };

export type ChatState = {
  sessionId: string;
  /** History items first, then live ones (keys starting with "live:"), oldest first. */
  items: Item[];
  historyLoaded: boolean;
  /** Highest event seq applied; 2b resubscribes from it. */
  lastSeq: number;
  run: { id: string; status: RunStatus } | null;
  currentTool: string | null;
};

export type Action =
  | { type: "history"; messages: HistoryMessage[] }
  | { type: "send"; clientMsgId: string; text: string }
  | { type: "server"; msg: ServerMessage }
  | { type: "dismiss"; key: string };

export const MAX_ITEMS = 1000;

export const initialChat = (sessionId: string): ChatState => ({
  sessionId,
  items: [],
  historyLoaded: false,
  lastSeq: 0,
  run: null,
  currentTool: null,
});

/** Errors that leave the message unsent; its text stays in the composer. */
const PROMPT_ERRORS: ReadonlySet<ErrorCode> = new Set(["busy", "read_only", "rate_limited", "hermes_unavailable", "hermes_error", "bad_request", "too_large", "not_found", "internal"]);

const NOTICE_TEXT: Partial<Record<ErrorCode, string>> = {
  busy: "Hermes is still working on the previous message. Wait for it to finish.",
  read_only: "This session belongs to another surface and is read-only here.",
  rate_limited: "Too many messages in a minute. Wait a little and send again.",
  hermes_unavailable: "Hermes is unavailable right now. Try again later.",
};

function historyItems(messages: HistoryMessage[]): Item[] {
  const items: Item[] = [];
  const toolByCall = new Map<string, Extract<Item, { kind: "tool" }>>();
  for (const m of messages) {
    const key = `h:${m.id}`;
    if (m.role === "user") {
      items.push({ kind: "user", key, text: m.content, status: "sent" });
    } else if (m.role === "assistant") {
      if (m.content.trim()) items.push({ kind: "assistant", key, text: m.content });
      m.toolCalls.forEach((c, i) => {
        const tool: Extract<Item, { kind: "tool" }> = {
          kind: "tool",
          key: `${key}:${i}`,
          tool: c.name,
          args: c.arguments || undefined,
          error: false,
          done: true,
          callId: c.id ?? undefined,
        };
        items.push(tool);
        if (c.id) toolByCall.set(c.id, tool);
      });
    } else {
      const tool = m.toolCallId ? toolByCall.get(m.toolCallId) : undefined;
      const result = m.content.slice(0, 4000);
      if (tool) tool.result = result;
      else items.push({ kind: "tool", key, tool: m.toolName ?? "tool", result, error: false, done: true });
    }
  }
  return items;
}

const cap = (items: Item[]) => (items.length > MAX_ITEMS ? items.slice(items.length - MAX_ITEMS) : items);

function updateLast<K extends Item["kind"]>(
  items: Item[],
  kind: K,
  match: (i: Extract<Item, { kind: K }>) => boolean,
  patch: (i: Extract<Item, { kind: K }>) => Partial<Extract<Item, { kind: K }>>,
): Item[] | null {
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i]!;
    if (it.kind === kind && match(it as Extract<Item, { kind: K }>)) {
      const next = items.slice();
      next[i] = { ...it, ...patch(it as Extract<Item, { kind: K }>) } as Item;
      return next;
    }
  }
  return null;
}

function applyEvent(s: ChatState, e: EventMessage): ChatState {
  // The relay sends a session's events in seq order (replay, then live), so anything at or below lastSeq is a repeat.
  if (e.seq <= s.lastSeq) return s;
  const base: ChatState = { ...s, lastSeq: e.seq };
  const key = `live:${e.seq}`;
  const running = (st: ChatState): ChatState => (st.run?.id === e.run_id ? st : { ...st, run: { id: e.run_id, status: "running" } });
  const push = (st: ChatState, item: Item): ChatState => ({ ...st, items: cap([...st.items, item]) });
  const end = (st: ChatState): ChatState => ({ ...st, run: null, currentTool: null });

  switch (e.kind) {
    case "tool.started":
      return push({ ...running(base), currentTool: e.data.tool }, {
        kind: "tool",
        key,
        tool: e.data.tool,
        args: e.data.preview,
        error: false,
        done: false,
        runId: e.run_id,
      });
    case "tool.completed": {
      const patch = { result: e.data.preview, duration: e.data.duration, error: e.data.error, done: true };
      const items = updateLast(base.items, "tool", (t) => !t.done && t.tool === e.data.tool && t.runId === e.run_id, () => patch);
      const st = { ...running(base), currentTool: null };
      return items ? { ...st, items } : push(st, { kind: "tool", key, tool: e.data.tool, runId: e.run_id, ...patch });
    }
    case "subagent.started":
      return push(running(base), { kind: "subagent", key, id: e.data.subagent_id, goal: e.data.goal, done: false });
    case "subagent.completed": {
      const patch = { status: e.data.status, summary: e.data.summary, duration: e.data.duration, done: true };
      const items = updateLast(base.items, "subagent", (a) => a.id === e.data.subagent_id && !a.done, () => patch);
      return items ? { ...running(base), items } : push(running(base), { kind: "subagent", key, id: e.data.subagent_id, ...patch });
    }
    case "approval.requested":
      // shortcut: approvals are phase 3; until then the run waits for an answer from another surface.
      return push(running(base), { kind: "status", key, tone: "muted", text: "Hermes is waiting for an approval (not supported in this version yet)." });
    case "answer":
      return push(base, { kind: "assistant", key, text: e.data.text, truncated: e.data.truncated });
    case "run.completed":
      return end(base);
    case "run.failed":
      return end(push(base, { kind: "status", key, tone: "error", text: e.data.error ? `Run failed: ${e.data.error}` : "Run failed." }));
    case "run.cancelled":
      return end(push(base, { kind: "status", key, tone: "muted", text: "Run cancelled." }));
    case "reconnected":
      return push(base, { kind: "status", key, tone: "muted", text: "The relay lost Hermes' stream for a moment; some steps may be missing." });
    case "approval.resolved":
    case "steer.queued":
    case "unknown":
      return base;
  }
}

function applyServer(s: ChatState, msg: ServerMessage): ChatState {
  switch (msg.type) {
    case "event":
      return msg.session_id === s.sessionId ? applyEvent(s, msg) : s;
    case "run":
      if (msg.session_id !== s.sessionId) return s;
      if (msg.status === "running" || msg.status === "waiting_for_approval") return { ...s, run: { id: msg.run_id, status: msg.status } };
      return s.run?.id === msg.run_id ? { ...s, run: null, currentTool: null } : s;
    case "ack": {
      const items = updateLast(s.items, "user", (u) => u.clientMsgId === msg.client_msg_id, () => ({ status: "sent", notice: undefined }));
      if (!items) return s;
      return { ...s, items, run: s.run?.id === msg.run_id ? s.run : { id: msg.run_id, status: "running" } };
    }
    case "error": {
      if (!msg.ref || !PROMPT_ERRORS.has(msg.code)) return s;
      const notice = { code: msg.code, message: NOTICE_TEXT[msg.code] ?? msg.message };
      const items = updateLast(
        s.items,
        "user",
        (u) => u.clientMsgId === msg.ref && u.status === "pending",
        () => ({ status: "failed", notice }),
      );
      return items ? { ...s, items } : s;
    }
    default:
      return s;
  }
}

export function chatReducer(s: ChatState, a: Action): ChatState {
  switch (a.type) {
    case "history":
      return { ...s, historyLoaded: true, items: cap([...historyItems(a.messages), ...s.items.filter((i) => i.key.startsWith("live:"))]) };
    case "send":
      return {
        ...s,
        items: cap([...s.items, { kind: "user", key: `live:u:${a.clientMsgId}`, text: a.text, status: "pending", clientMsgId: a.clientMsgId }]),
      };
    case "server":
      return applyServer(s, a.msg);
    case "dismiss":
      // A failed message's text is still in the composer, so dismissing its notice drops the bubble too.
      return { ...s, items: s.items.filter((i) => !(i.key === a.key && i.kind === "user" && i.status === "failed")) };
  }
}

/** True while a turn is going or a prompt waits for its ack: the composer is locked. */
export const isBusy = (s: ChatState) => s.run !== null || s.items.some((i) => i.kind === "user" && i.status === "pending");
