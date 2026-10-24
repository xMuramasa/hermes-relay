// REST client for the relay (Bearer device token). Responses are untrusted: each one is checked and bounded here.
import { ERROR_CODES, isId, isObject, type ErrorCode } from "./protocol";

/** The protocol's error codes, plus `unreachable` (no answer from the relay: network down or timed out). */
export type ApiErrorCode = ErrorCode | "unreachable";

export class ApiError extends Error {
  constructor(
    readonly code: ApiErrorCode,
    message: string,
    readonly status = 0,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export type Fetch = (input: string, init?: RequestInit) => Promise<Response>;

export const TIMEOUT_MS = 15_000;
const MAX_SESSIONS = 200;
const MAX_MESSAGES = 500;
const MAX_CONTENT = 262144;
const MAX_PREVIEW = 4000;

// The relay's status for each code (relay/internal/server/server.go), for error bodies that aren't its JSON.
const CODE_OF_STATUS: Record<number, ErrorCode> = {
  400: "bad_request",
  401: "unauthorized",
  403: "read_only",
  404: "not_found",
  409: "busy",
  410: "replay_gap",
  413: "too_large",
  429: "rate_limited",
  502: "hermes_error",
  503: "hermes_unavailable",
};

type RequestOptions = { method?: string; token?: string; body?: unknown; fetchFn?: Fetch; timeoutMs?: number };

/** One JSON request to the relay. Throws ApiError; returns the parsed body (undefined when empty). */
export async function request(baseUrl: string, path: string, opts: RequestOptions = {}): Promise<unknown> {
  const { method = "GET", token, body, fetchFn = fetch, timeoutMs = TIMEOUT_MS } = opts;
  const headers: Record<string, string> = { Accept: "application/json" };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeoutMs);
  let res: Response;
  let text: string;
  try {
    res = await fetchFn(baseUrl + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: ctrl.signal,
    });
    text = await res.text();
  } catch {
    throw new ApiError("unreachable", ctrl.signal.aborted ? "The relay didn't answer in time." : "Can't reach the relay.");
  } finally {
    clearTimeout(timer);
  }
  let json: unknown;
  try {
    json = text ? JSON.parse(text) : undefined;
  } catch {
    json = undefined;
  }
  if (!res.ok) {
    const err = isObject(json) && isObject(json.error) ? json.error : undefined;
    const code = (ERROR_CODES as readonly unknown[]).includes(err?.code) ? (err!.code as ErrorCode) : CODE_OF_STATUS[res.status] ?? "internal";
    const message = typeof err?.message === "string" ? err.message.slice(0, 512) : `The relay answered ${res.status}.`;
    throw new ApiError(code, message, res.status);
  }
  if (text && json === undefined) throw new ApiError("hermes_error", "The relay sent an invalid response.", res.status);
  return json;
}

// --- Hermes objects, as the relay passes them through ---

export type Session = {
  id: string;
  source: string;
  title: string | null;
  preview: string | null;
  startedAt: number | null; // unix seconds
  lastActive: number | null; // unix seconds
  messageCount: number;
};

export type ToolCall = { id: string | null; name: string; arguments: string };
export type HistoryMessage = {
  id: string;
  role: "user" | "assistant" | "tool";
  content: string;
  toolCalls: ToolCall[];
  toolCallId: string | null;
  toolName: string | null;
  timestamp: number | null;
};

const strOrNull = (v: unknown, max: number) => (typeof v === "string" ? v.slice(0, max) : null);
const numOrNull = (v: unknown) => (typeof v === "number" && Number.isFinite(v) ? v : null);

export function parseSession(v: unknown): Session | null {
  if (!isObject(v) || !isId(v.id)) return null;
  return {
    id: v.id,
    source: strOrNull(v.source, 32) ?? "unknown",
    title: strOrNull(v.title, 200),
    preview: strOrNull(v.preview, 200),
    startedAt: numOrNull(v.started_at),
    lastActive: numOrNull(v.last_active) ?? numOrNull(v.ended_at) ?? numOrNull(v.started_at),
    messageCount: Math.max(0, Math.floor(numOrNull(v.message_count) ?? 0)),
  };
}

function parseToolCalls(v: unknown): ToolCall[] {
  if (!Array.isArray(v)) return [];
  return v.slice(0, 50).flatMap((c) => {
    if (!isObject(c) || !isObject(c.function) || typeof c.function.name !== "string") return [];
    return [
      {
        id: strOrNull(c.id, 128),
        name: c.function.name.slice(0, 128),
        arguments: strOrNull(c.function.arguments, MAX_PREVIEW) ?? "",
      },
    ];
  });
}

export function parseMessage(v: unknown): HistoryMessage | null {
  if (!isObject(v) || (v.role !== "user" && v.role !== "assistant" && v.role !== "tool")) return null;
  if (typeof v.id !== "number" && typeof v.id !== "string") return null;
  return {
    id: String(v.id).slice(0, 64),
    role: v.role,
    content: strOrNull(v.content, MAX_CONTENT) ?? "",
    toolCalls: parseToolCalls(v.tool_calls),
    toolCallId: strOrNull(v.tool_call_id, 128),
    toolName: strOrNull(v.tool_name, 128),
    timestamp: numOrNull(v.timestamp),
  };
}

function dataList(body: unknown): unknown[] {
  if (!isObject(body) || !Array.isArray(body.data)) throw new ApiError("hermes_error", "The relay sent an unexpected response.");
  return body.data;
}

function sessionPath(id: string) {
  if (!isId(id)) throw new ApiError("not_found", "Unknown session.");
  return `/api/sessions/${encodeURIComponent(id)}`;
}

export type Creds = { url: string; token: string; device: string };

export function createApi(creds: Pick<Creds, "url" | "token">, fetchFn?: Fetch) {
  const call = (path: string, method = "GET") => request(creds.url, path, { method, token: creds.token, fetchFn });
  return {
    async listSessions(): Promise<Session[]> {
      const list = dataList(await call("/api/sessions?limit=100"));
      return list.slice(0, MAX_SESSIONS).flatMap((s) => parseSession(s) ?? []);
    },
    async createSession(): Promise<Session> {
      const body = await request(creds.url, "/api/sessions", { method: "POST", token: creds.token, body: {}, fetchFn });
      const s = parseSession(isObject(body) ? body.session : undefined);
      if (!s) throw new ApiError("hermes_error", "The relay sent an unexpected response.");
      return s;
    },
    async deleteSession(id: string): Promise<void> {
      await call(sessionPath(id), "DELETE");
    },
    async getMessages(id: string): Promise<HistoryMessage[]> {
      const list = dataList(await call(`${sessionPath(id)}/messages`));
      return list.slice(-MAX_MESSAGES).flatMap((m) => parseMessage(m) ?? []);
    },
  };
}
export type Api = ReturnType<typeof createApi>;
