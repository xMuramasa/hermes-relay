import { describe, expect, test } from "bun:test";
import Ajv2020 from "ajv/dist/2020";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { authMsg, decodeServerMessage, pingMsg, promptMsg, subscribeMsg, unsubscribeMsg } from "../src/lib/protocol";

const PROTOCOL = join(import.meta.dir, "../../../protocol");
const PHONE_TYPES = new Set(["auth", "subscribe", "unsubscribe", "prompt", "stop", "steer", "approve", "ping"]);

function examples(dir: "valid" | "invalid") {
  const path = join(PROTOCOL, "examples", dir);
  return readdirSync(path)
    .filter((f) => f.endsWith(".json"))
    .map((f) => ({ name: f, raw: readFileSync(join(path, f), "utf8") }));
}
const typeOf = (raw: string) => {
  const v = JSON.parse(raw);
  return v && typeof v === "object" && !Array.isArray(v) ? v.type : undefined;
};
const fromPhone = (raw: string) => PHONE_TYPES.has(typeOf(raw));

describe("decodeServerMessage against protocol/examples", () => {
  const valid = examples("valid").filter((e) => !fromPhone(e.raw));
  const invalid = examples("invalid").filter((e) => !fromPhone(e.raw));

  test("there are relay → phone examples", () => {
    expect(valid.length).toBeGreaterThan(20);
    expect(invalid.length).toBeGreaterThan(10);
  });

  for (const e of valid) {
    test(`accepts valid/${e.name}`, () => {
      const d = decodeServerMessage(e.raw);
      expect(d).toEqual({ ok: true, msg: JSON.parse(e.raw) });
    });
  }
  for (const e of invalid) {
    test(`rejects invalid/${e.name}`, () => {
      expect(decodeServerMessage(e.raw).ok).toBe(false);
    });
  }

  test("rejects phone → relay messages, non-JSON and oversize frames", () => {
    for (const e of examples("valid").filter((x) => fromPhone(x.raw))) expect(decodeServerMessage(e.raw).ok).toBe(false);
    expect(decodeServerMessage("{nope").ok).toBe(false);
    expect(decodeServerMessage(JSON.stringify({ type: "pong", pad: "x".repeat(3 * 1024 * 1024) })).ok).toBe(false);
  });

  test("rejects inherited-looking keys and extra fields", () => {
    expect(decodeServerMessage('{"type":"pong","constructor":1}').ok).toBe(false);
    expect(decodeServerMessage('{"type":"toString"}').ok).toBe(false);
  });

  test("counts lengths in code points like JSON Schema", () => {
    // 64 emoji = 128 UTF-16 units but 64 code points: within ready.device's maxLength of 64.
    const device = "🜂".repeat(64);
    expect(decodeServerMessage(JSON.stringify({ type: "ready", device, server_version: "1" })).ok).toBe(true);
    expect(decodeServerMessage(JSON.stringify({ type: "ready", device: device + "x", server_version: "1" })).ok).toBe(false);
  });
});

describe("builders validate against protocol/schema.json", () => {
  const ajv = new Ajv2020({ strict: false });
  const validate = ajv.compile(JSON.parse(readFileSync(join(PROTOCOL, "schema.json"), "utf8")));
  const built = {
    auth: authMsg("trs_FAKE0000000000000000000000000000000000000000"),
    subscribe: subscribeMsg("api_1792801001_b6235dd4"),
    subscribeAfter: subscribeMsg("api_1792801001_b6235dd4", 42),
    unsubscribe: unsubscribeMsg("api_1792801001_b6235dd4"),
    prompt: promptMsg("6f1c2b8e-1d2a-4c55-9a0e-1b2c3d4e5f60", "api_1792801001_b6235dd4", "hello"),
    ping: pingMsg(),
  };
  for (const [name, msg] of Object.entries(built)) {
    test(name, () => {
      expect(validate(msg)).toBe(true);
    });
  }
  test("the schema itself rejects a bad build (sanity)", () => {
    expect(validate(promptMsg("x", "a/b", "hi"))).toBe(false);
  });
});
