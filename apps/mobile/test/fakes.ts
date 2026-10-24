// Test doubles: a fetch that records calls, and a WebSocket the test drives by hand.
import type { Fetch } from "../src/lib/api";
import type { WebSocketLike } from "../src/lib/socket";

export const json = (status: number, body: unknown) =>
  new Response(body === undefined ? "" : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

export function fakeFetch(handler: (url: string, init?: RequestInit) => Response | Promise<Response>) {
  const calls: { url: string; init?: RequestInit }[] = [];
  const fn: Fetch = async (url, init) => {
    calls.push({ url, init });
    return handler(url, init);
  };
  return { fn, calls };
}

export class FakeWebSocket implements WebSocketLike {
  static instances: FakeWebSocket[] = [];
  readyState = 0;
  sent: unknown[] = [];
  closed = false;
  onopen: WebSocketLike["onopen"] = null;
  onmessage: WebSocketLike["onmessage"] = null;
  onclose: WebSocketLike["onclose"] = null;
  onerror: WebSocketLike["onerror"] = null;

  constructor(readonly url: string) {
    FakeWebSocket.instances.push(this);
  }
  send(data: string) {
    this.sent.push(JSON.parse(data));
  }
  close() {
    this.closed = true;
    this.readyState = 3;
  }
  // test helpers
  open() {
    this.readyState = 1;
    this.onopen?.({});
  }
  receive(msg: unknown) {
    this.onmessage?.({ data: typeof msg === "string" ? msg : JSON.stringify(msg) });
  }
  serverClose() {
    this.readyState = 3;
    this.onclose?.({});
  }
}
