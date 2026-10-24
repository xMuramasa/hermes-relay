// The one WebSocket to the relay: auth first, subscribe after ready, app-level ping.
// shortcut: no automatic reconnect or replay from the last seq; reconnect() is called on user action. Phase 2b adds
// backoff, foreground resume and `after_seq`.
import { authMsg, decodeServerMessage, pingMsg, subscribeMsg, unsubscribeMsg, type ClientMessage, type ServerMessage } from "./protocol";

export type ConnState = "disconnected" | "connecting" | "connected";

/** The bits of the WebSocket API used here, so tests can pass a fake. */
export interface WebSocketLike {
  readyState: number;
  onopen: ((ev: unknown) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onclose: ((ev: unknown) => void) | null;
  onerror: ((ev: unknown) => void) | null;
  send(data: string): void;
  close(code?: number, reason?: string): void;
}
export type WebSocketCtor = new (url: string) => WebSocketLike;

export type SocketOptions = {
  url: string; // the relay's https:// URL
  token: string;
  onMessage: (msg: ServerMessage) => void;
  onState: (state: ConnState) => void;
  WebSocketImpl?: WebSocketCtor;
  pingMs?: number;
  readyTimeoutMs?: number;
};

const OPEN = 1;

export function wsUrl(httpsUrl: string): string {
  return httpsUrl.replace(/^https:/, "wss:").replace(/\/+$/, "") + "/ws";
}

export class RelaySocket {
  private ws: WebSocketLike | null = null;
  private state: ConnState = "disconnected";
  private subs = new Set<string>();
  private pingTimer: ReturnType<typeof setInterval> | null = null;
  private readyTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(private opts: SocketOptions) {}

  get connState() {
    return this.state;
  }

  /** Opens the socket unless one is open or opening. */
  connect() {
    if (this.ws) return;
    const Impl = this.opts.WebSocketImpl ?? (globalThis.WebSocket as unknown as WebSocketCtor);
    const ws = new Impl(wsUrl(this.opts.url));
    this.ws = ws;
    this.setState("connecting");
    ws.onopen = () => {
      ws.send(JSON.stringify(authMsg(this.opts.token)));
      this.readyTimer = setTimeout(() => this.drop(ws), this.opts.readyTimeoutMs ?? 10_000);
    };
    ws.onmessage = (ev) => {
      if (this.ws !== ws || typeof ev.data !== "string") return;
      const d = decodeServerMessage(ev.data);
      if (!d.ok) return; // untrusted input: drop what doesn't match the schema
      if (d.msg.type === "ready" && this.state === "connecting") {
        this.clearReadyTimer();
        this.setState("connected");
        for (const id of this.subs) this.send(subscribeMsg(id));
        this.pingTimer = setInterval(() => this.send(pingMsg()), this.opts.pingMs ?? 20_000);
      }
      this.opts.onMessage(d.msg);
    };
    ws.onerror = () => {};
    ws.onclose = () => {
      if (this.ws === ws) this.drop(ws);
    };
  }

  /** Sends when authenticated; false otherwise. */
  send(msg: ClientMessage): boolean {
    if (this.state !== "connected" || !this.ws || this.ws.readyState !== OPEN) return false;
    this.ws.send(JSON.stringify(msg));
    return true;
  }

  subscribe(sessionId: string) {
    this.subs.add(sessionId);
    this.send(subscribeMsg(sessionId));
  }

  unsubscribe(sessionId: string) {
    if (this.subs.delete(sessionId)) this.send(unsubscribeMsg(sessionId));
  }

  /** Closes for good (unpair, sign-out). */
  close() {
    this.subs.clear();
    if (this.ws) this.drop(this.ws);
  }

  private drop(ws: WebSocketLike) {
    this.clearReadyTimer();
    if (this.pingTimer) clearInterval(this.pingTimer);
    this.pingTimer = null;
    ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
    try {
      ws.close();
    } catch {
      // already closed
    }
    if (this.ws === ws) this.ws = null;
    this.setState("disconnected");
  }

  private clearReadyTimer() {
    if (this.readyTimer) clearTimeout(this.readyTimer);
    this.readyTimer = null;
  }

  private setState(s: ConnState) {
    if (s === this.state) return;
    this.state = s;
    this.opts.onState(s);
  }
}
