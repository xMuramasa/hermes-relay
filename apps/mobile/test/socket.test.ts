import { beforeEach, expect, test } from "bun:test";
import type { ServerMessage } from "../src/lib/protocol";
import { RelaySocket, wsUrl, type ConnState } from "../src/lib/socket";
import { FakeWebSocket } from "./fakes";

const TOKEN = "trs_FAKE" + "C".repeat(39);
const SID = "api_1792801001_b6235dd4";

function setup(pingMs = 20_000) {
  const states: ConnState[] = [];
  const msgs: ServerMessage[] = [];
  const sock = new RelaySocket({
    url: "https://relay.example/",
    token: TOKEN,
    onMessage: (m) => msgs.push(m),
    onState: (s) => states.push(s),
    WebSocketImpl: FakeWebSocket,
    pingMs,
  });
  return { sock, states, msgs };
}
const last = () => FakeWebSocket.instances.at(-1)!;

beforeEach(() => {
  FakeWebSocket.instances = [];
});

test("wsUrl", () => {
  expect(wsUrl("https://relay.example/")).toBe("wss://relay.example/ws");
});

test("auth first, subscribe only after ready", () => {
  const { sock, states, msgs } = setup();
  sock.subscribe(SID); // before connecting: remembered, not sent
  sock.connect();
  const ws = last();
  expect(ws.url).toBe("wss://relay.example/ws");
  expect(states).toEqual(["connecting"]);
  ws.open();
  expect(ws.sent).toEqual([{ type: "auth", token: TOKEN }]);
  expect(sock.send({ type: "ping" })).toBe(false); // not authenticated yet
  ws.receive({ type: "ready", device: "pixel-8", server_version: "0.1.0" });
  expect(states).toEqual(["connecting", "connected"]);
  expect(ws.sent.slice(1)).toEqual([{ type: "subscribe", session_id: SID }]);
  expect(msgs[0]!.type).toBe("ready");
  sock.close();
});

test("drops frames that don't decode and forwards the rest", () => {
  const { sock, msgs } = setup();
  sock.connect();
  last().open();
  last().receive({ type: "ready", device: "d", server_version: "1" });
  last().receive("not json");
  last().receive({ type: "event", kind: "message.delta" });
  last().receive({ type: "pong" });
  expect(msgs.map((m) => m.type)).toEqual(["ready", "pong"]);
  sock.close();
});

test("pings on an interval while connected", async () => {
  const { sock } = setup(10);
  sock.connect();
  last().open();
  last().receive({ type: "ready", device: "d", server_version: "1" });
  await Bun.sleep(35);
  expect(last().sent.filter((m) => (m as { type: string }).type === "ping").length).toBeGreaterThanOrEqual(2);
  sock.close();
});

test("a close leaves it disconnected until connect() is called again", () => {
  const { sock, states } = setup();
  sock.subscribe(SID);
  sock.connect();
  last().open();
  last().receive({ type: "ready", device: "d", server_version: "1" });
  last().serverClose();
  expect(sock.connState).toBe("disconnected");
  expect(FakeWebSocket.instances.length).toBe(1); // no automatic reconnect in 2a
  sock.connect();
  last().open();
  last().receive({ type: "ready", device: "d", server_version: "1" });
  expect(last().sent).toEqual([
    { type: "auth", token: TOKEN },
    { type: "subscribe", session_id: SID },
  ]);
  expect(states).toEqual(["connecting", "connected", "disconnected", "connecting", "connected"]);
  sock.close();
});

test("unauthorized before ready: error forwarded, socket closed", () => {
  const { sock, msgs } = setup();
  sock.connect();
  last().open();
  last().receive({ type: "error", code: "unauthorized", message: "unauthorized" });
  last().serverClose();
  expect(msgs).toEqual([{ type: "error", code: "unauthorized", message: "unauthorized" }]);
  expect(sock.connState).toBe("disconnected");
});

test("unsubscribe sends unsubscribe and forgets the session", () => {
  const { sock } = setup();
  sock.subscribe(SID);
  sock.connect();
  last().open();
  last().receive({ type: "ready", device: "d", server_version: "1" });
  sock.unsubscribe(SID);
  expect(last().sent.at(-1)).toEqual({ type: "unsubscribe", session_id: SID });
  sock.close();
  expect(last().closed).toBe(true);
});
