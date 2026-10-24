import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { ApiError, createApi, request } from "../src/lib/api";
import { fakeFetch, json } from "./fakes";

/** The ApiError a call must fail with. */
const fail = (p: Promise<unknown>) =>
  p.then(
    () => {
      throw new Error("expected an ApiError");
    },
    (e: ApiError) => e,
  );

const CREDS = { url: "https://relay.example", token: "trs_FAKE" + "B".repeat(39) };

function fixture(kind: string) {
  const lines = readFileSync(join(import.meta.dir, "../../../probe/fixtures/tool.jsonl"), "utf8").split("\n");
  return JSON.parse(lines.find((l) => l.includes(`"kind": "${kind}"`))!).body;
}

describe("REST client", () => {
  test("sends the Bearer token and parses sessions", async () => {
    const session = fixture("session.create").session;
    const f = fakeFetch(() => json(200, { data: [session, { id: "bad/id" }, { ...session, id: "cli_1", source: "cli", title: null }], has_more: false }));
    const list = await createApi(CREDS, f.fn).listSessions();
    expect(new Headers(f.calls[0]!.init?.headers).get("Authorization")).toBe(`Bearer ${CREDS.token}`);
    expect(f.calls[0]!.url).toBe("https://relay.example/api/sessions?limit=100");
    expect(list.map((s) => [s.id, s.source, s.title])).toEqual([
      ["api_1792801001_b6235dd4", "api_server", "trismegistus probe: tool"],
      ["cli_1", "cli", null],
    ]);
  });

  test("creates a session with an empty body", async () => {
    const f = fakeFetch(() => json(201, fixture("session.create")));
    const s = await createApi(CREDS, f.fn).createSession();
    expect(s.id).toBe("api_1792801001_b6235dd4");
    expect(f.calls[0]!.init?.method).toBe("POST");
    expect(f.calls[0]!.init?.body).toBe("{}");
  });

  test("gets messages from a real recording", async () => {
    const f = fakeFetch(() => json(200, fixture("session.messages")));
    const msgs = await createApi(CREDS, f.fn).getMessages("api_1792801001_b6235dd4");
    expect(f.calls[0]!.url).toBe("https://relay.example/api/sessions/api_1792801001_b6235dd4/messages");
    expect(msgs.map((m) => m.role)).toEqual(["user", "assistant", "tool", "assistant"]);
    expect(msgs[1]!.toolCalls[0]!.name).toBe("terminal");
  });

  test("deletes, and refuses ids that aren't ids before any request", async () => {
    const f = fakeFetch(() => new Response(null, { status: 204 }));
    const api = createApi(CREDS, f.fn);
    await api.deleteSession("api_1");
    expect(f.calls[0]!.init?.method).toBe("DELETE");
    expect((await fail(api.deleteSession("../x"))).code).toBe("not_found");
    expect(f.calls.length).toBe(1);
  });

  test("maps relay JSON errors to protocol codes", async () => {
    const f = fakeFetch(() => json(403, { error: { code: "read_only", message: "this session is read-only here" } }));
    const err = await createApi(CREDS, f.fn).deleteSession("telegram_1").catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect([err.code, err.message, err.status]).toEqual(["read_only", "this session is read-only here", 403]);
  });

  test("maps non-JSON errors by status (e.g. Caddy's 502)", async () => {
    const f = fakeFetch(() => new Response("<html>bad gateway</html>", { status: 502 }));
    expect((await fail(request("https://r.example", "/api/sessions", { fetchFn: f.fn }))).code).toBe("hermes_error");
    const g = fakeFetch(() => new Response("nope", { status: 418 }));
    expect((await fail(request("https://r.example", "/x", { fetchFn: g.fn }))).code).toBe("internal");
  });

  test("times out after the given time", async () => {
    const hang = (_u: string, init?: RequestInit) =>
      new Promise<Response>((_, reject) => init?.signal?.addEventListener("abort", () => reject(new Error("aborted"))));
    const err = await fail(request("https://r.example", "/api/sessions", { fetchFn: hang, timeoutMs: 20 }));
    expect(err.code).toBe("unreachable");
    expect(err.message).toContain("in time");
  });

  test("rejects a response that isn't the expected shape", async () => {
    const f = fakeFetch(() => json(200, { sessions: [] }));
    expect((await fail(createApi(CREDS, f.fn).listSessions())).code).toBe("hermes_error");
  });
});
