import { describe, expect, test } from "bun:test";
import { ApiError } from "../src/lib/api";
import { normalizeCode, normalizeUrl, pairingErrorText, parsePairingPayload, redeem } from "../src/lib/pairing";
import { fakeFetch, json } from "./fakes";

const TOKEN = "trs_FAKE" + "A".repeat(39);

describe("parsePairingPayload", () => {
  test("good payload", () => {
    expect(parsePairingPayload('{"v":1,"url":"https://relay.example/","code":"abcd-efgh-jkmn"}')).toEqual({
      ok: true,
      url: "https://relay.example",
      code: "ABCDEFGHJKMN",
    });
  });
  test("rejects http://", () => {
    expect(parsePairingPayload('{"v":1,"url":"http://relay.example","code":"ABCD-EFGH-JKMN"}').ok).toBe(false);
  });
  test("rejects bad JSON and other QR codes", () => {
    expect(parsePairingPayload("{v:1").ok).toBe(false);
    expect(parsePairingPayload("https://example.com").ok).toBe(false);
    expect(parsePairingPayload('{"v":2,"url":"https://relay.example","code":"ABCD-EFGH-JKMN"}').ok).toBe(false);
  });
  test("rejects a missing or malformed code", () => {
    expect(parsePairingPayload('{"v":1,"url":"https://relay.example"}').ok).toBe(false);
    expect(parsePairingPayload('{"v":1,"url":"https://relay.example","code":"ABCD-EFGH-JKMI"}').ok).toBe(false); // I isn't Crockford
    expect(parsePairingPayload('{"v":1,"url":"https://relay.example","code":"ABCD-EFGH"}').ok).toBe(false);
  });
});

test("normalizeCode and normalizeUrl", () => {
  expect(normalizeCode(" 0123 4567-89ab ")).toBe("0123456789AB");
  expect(normalizeUrl("https://user@evil.example")).toBeNull();
  expect(normalizeUrl("https://relay.example/?x=1")).toBeNull();
  expect(normalizeUrl("https://10.0.0.5:8443/relay/")).toBe("https://10.0.0.5:8443/relay");
});

describe("redeem", () => {
  test("posts the code and returns the credentials", async () => {
    const f = fakeFetch(() => json(200, { token: TOKEN, device: { name: "pixel-8", created_at: "2026-10-24T10:00:00Z" } }));
    expect(await redeem("https://relay.example", "ABCDEFGHJKMN", f.fn)).toEqual({ url: "https://relay.example", token: TOKEN, device: "pixel-8" });
    expect(f.calls[0]!.url).toBe("https://relay.example/api/pair");
    expect(f.calls[0]!.init?.method).toBe("POST");
    expect(JSON.parse(String(f.calls[0]!.init?.body))).toEqual({ code: "ABCDEFGHJKMN" });
    expect(new Headers(f.calls[0]!.init?.headers).get("Authorization")).toBeNull();
  });
  test("an expired or used code is unauthorized, with plain text", async () => {
    const f = fakeFetch(() => json(401, { error: { code: "unauthorized", message: "invalid, used or expired pairing code" } }));
    const err = await redeem("https://relay.example", "ABCDEFGHJKMN", f.fn).catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err.code).toBe("unauthorized");
    expect(pairingErrorText(err)).toContain("expired");
  });
  test("an unreachable relay", async () => {
    const err = await redeem("https://relay.example", "ABCDEFGHJKMN", () => Promise.reject(new TypeError("Network request failed"))).catch((e) => e);
    expect(err.code).toBe("unreachable");
    expect(pairingErrorText(err)).toContain("reach");
  });
  test("a malformed token is refused", async () => {
    const f = fakeFetch(() => json(200, { token: "nope", device: { name: "pixel-8" } }));
    expect((await redeem("https://relay.example", "ABCDEFGHJKMN", f.fn).catch((e) => e)).code).toBe("hermes_error");
  });
});
