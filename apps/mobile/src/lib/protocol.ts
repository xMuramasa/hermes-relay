// Types, decoder and builders for the WebSocket protocol in protocol/schema.json (the contract).
// The decoder is hand-written rather than ajv: ajv compiles validators with `new Function`, which Hermes doesn't run.
// test/protocol.test.ts checks it against every example in protocol/examples/.

export const ERROR_CODES = [
  "unauthorized",
  "bad_request",
  "too_large",
  "rate_limited",
  "busy",
  "not_found",
  "read_only",
  "replay_gap",
  "hermes_unavailable",
  "hermes_error",
  "internal",
] as const;
export type ErrorCode = (typeof ERROR_CODES)[number];

export const RUN_STATUSES = ["running", "waiting_for_approval", "completed", "failed", "cancelled"] as const;
export type RunStatus = (typeof RUN_STATUSES)[number];

export const CHOICES = ["once", "session", "deny"] as const;
export type Choice = (typeof CHOICES)[number];

export type EventData = {
  "tool.started": { tool: string; preview?: string };
  "tool.completed": { tool: string; error: boolean; duration?: number; preview?: string };
  "subagent.started": {
    subagent_id: string;
    goal?: string;
    model?: string;
    child_session_id?: string;
    task_index?: number;
    task_count?: number;
  };
  "subagent.completed": { subagent_id: string; status?: string; summary?: string; duration?: number };
  "approval.requested": { request_id: string; command: string; description?: string; choices: Choice[] };
  "approval.resolved": { request_id: string; choice: string };
  answer: { text: string; truncated?: boolean };
  "run.completed": Record<string, never>;
  "run.failed": { error?: string };
  "run.cancelled": { reason?: string };
  "steer.queued": Record<string, never>;
  reconnected: { reason: "stream_lost" | "relay_restart" };
  unknown: { event: string; data: Record<string, unknown>; truncated?: boolean };
};
export type EventKind = keyof EventData;

export type EventMessage = {
  [K in EventKind]: { type: "event"; session_id: string; run_id: string; seq: number; kind: K; data: EventData[K]; ts: number };
}[EventKind];
export type ReadyMessage = { type: "ready"; device: string; server_version: string };
export type RunMessage = { type: "run"; run_id: string; session_id: string; status: RunStatus };
export type AckMessage = { type: "ack"; client_msg_id: string; run_id: string; replayed: boolean };
export type ErrorMessage = { type: "error"; code: ErrorCode; message: string; ref?: string };
export type PongMessage = { type: "pong" };
export type ServerMessage = ReadyMessage | EventMessage | RunMessage | AckMessage | ErrorMessage | PongMessage;

export type ClientMessage =
  | { type: "auth"; token: string }
  | { type: "subscribe"; session_id: string; after_seq?: number }
  | { type: "unsubscribe"; session_id: string }
  | { type: "prompt"; client_msg_id: string; session_id: string; text: string }
  | { type: "stop"; run_id: string }
  | { type: "steer"; run_id: string; text: string }
  | { type: "approve"; run_id: string; request_id: string; choice: Choice }
  | { type: "ping" };

/** Biggest relay frame we parse: a 256 KiB answer plus JSON escaping and the envelope. */
export const MAX_SERVER_FRAME = 2 * 1024 * 1024;
export const MAX_TEXT = 32768;

// --- checks mirroring schema.json ---

type Check = (v: unknown) => boolean;
type Obj = Record<string, unknown>;

export const isObject = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v);

/** JSON Schema lengths count code points; UTF-16 length is an upper bound, so only count when it matters. */
function codePoints(s: string): number {
  let n = 0;
  for (const _ of s) n++;
  return n;
}
const str =
  (min: number, max: number, re?: RegExp): Check =>
  (v) => {
    if (typeof v !== "string") return false;
    const len = v.length <= max && v.length >= min * 2 ? v.length : codePoints(v);
    return len >= min && len <= max && (!re || re.test(v));
  };
const int = (min: number, max = Number.MAX_SAFE_INTEGER): Check => (v) => Number.isInteger(v) && (v as number) >= min && (v as number) <= max;
const oneOf = (values: readonly string[]): Check => (v) => typeof v === "string" && values.includes(v);
const bool: Check = (v) => typeof v === "boolean";

const ID_RE = /^[A-Za-z0-9][A-Za-z0-9_.:-]*$/;
const id = str(1, 128, ID_RE);
const short = str(0, 128);
const preview = str(0, 4000);
const long = str(0, 16000);
const duration: Check = (v) => typeof v === "number" && Number.isFinite(v) && v >= 0;

export const isId = (v: unknown): v is string => id(v);

/** An object with exactly these keys (required ones present), like `additionalProperties: false`. */
function shape(required: Record<string, Check>, optional: Record<string, Check> = {}): Check {
  return (v) => {
    if (!isObject(v)) return false;
    for (const k of Object.keys(v)) if (!Object.hasOwn(required, k) && !Object.hasOwn(optional, k)) return false;
    for (const [k, c] of Object.entries(required)) if (!Object.hasOwn(v, k) || !c(v[k])) return false;
    for (const [k, c] of Object.entries(optional)) if (Object.hasOwn(v, k) && !c(v[k])) return false;
    return true;
  };
}

const empty = shape({});
const choices: Check = (v) =>
  Array.isArray(v) && v.length >= 1 && v.length <= 3 && new Set(v).size === v.length && v.every(oneOf(CHOICES));

const dataChecks: Record<EventKind, Check> = {
  "tool.started": shape({ tool: short }, { preview }),
  "tool.completed": shape({ tool: short, error: bool }, { duration, preview }),
  "subagent.started": shape(
    { subagent_id: short },
    { goal: preview, model: short, child_session_id: short, task_index: int(0), task_count: int(0) },
  ),
  "subagent.completed": shape({ subagent_id: short }, { status: short, summary: long, duration }),
  "approval.requested": shape({ request_id: id, command: long, choices }, { description: preview }),
  "approval.resolved": shape({ request_id: id, choice: str(0, 32, /^[a-z_]+$/) }),
  answer: shape({ text: str(0, 262144) }, { truncated: bool }),
  "run.completed": empty,
  "run.failed": shape({}, { error: preview }),
  "run.cancelled": shape({}, { reason: short }),
  "steer.queued": empty,
  reconnected: shape({ reason: oneOf(["stream_lost", "relay_restart"]) }),
  unknown: shape({ event: str(0, 64), data: isObject }, { truncated: bool }),
};
const EVENT_KINDS = Object.keys(dataChecks) as EventKind[];

const present: Check = () => true; // `type` is matched before its check is picked

const messageChecks: Record<ServerMessage["type"], Check> = {
  ready: shape({ type: present, device: str(1, 64), server_version: str(1, 64) }),
  event: (v) => {
    const ok = shape({
      type: present,
      session_id: id,
      run_id: id,
      seq: int(1),
      kind: oneOf(EVENT_KINDS),
      data: isObject,
      ts: int(0, Number.MAX_VALUE),
    })(v);
    return ok && dataChecks[(v as Obj).kind as EventKind]((v as Obj).data);
  },
  run: shape({ type: present, run_id: id, session_id: id, status: oneOf(RUN_STATUSES) }),
  ack: shape({ type: present, client_msg_id: id, run_id: id, replayed: bool }),
  error: shape({ type: present, code: oneOf(ERROR_CODES), message: str(0, 512) }, { ref: id }),
  pong: shape({ type: present }),
};

export type Decoded = { ok: true; msg: ServerMessage } | { ok: false; error: string };

/** Parses and validates one relay → phone frame. Anything that doesn't match the schema is an error. */
export function decodeServerMessage(raw: string): Decoded {
  if (typeof raw !== "string") return { ok: false, error: "not a text frame" };
  if (raw.length > MAX_SERVER_FRAME) return { ok: false, error: "frame too large" };
  let v: unknown;
  try {
    v = JSON.parse(raw);
  } catch {
    return { ok: false, error: "not JSON" };
  }
  if (!isObject(v) || typeof v.type !== "string") return { ok: false, error: "not a message object" };
  const type = v.type;
  if (!Object.hasOwn(messageChecks, type)) return { ok: false, error: `unexpected type ${type.slice(0, 32)}` };
  if (!messageChecks[type as ServerMessage["type"]](v)) {
    return { ok: false, error: `invalid ${type} message` };
  }
  return { ok: true, msg: v as unknown as ServerMessage };
}

// --- builders for phone → relay messages ---

export const authMsg = (token: string): ClientMessage => ({ type: "auth", token });
export const subscribeMsg = (session_id: string, after_seq?: number): ClientMessage =>
  after_seq === undefined ? { type: "subscribe", session_id } : { type: "subscribe", session_id, after_seq };
export const unsubscribeMsg = (session_id: string): ClientMessage => ({ type: "unsubscribe", session_id });
export const promptMsg = (client_msg_id: string, session_id: string, text: string): ClientMessage => ({
  type: "prompt",
  client_msg_id,
  session_id,
  text,
});
export const pingMsg = (): ClientMessage => ({ type: "ping" });
