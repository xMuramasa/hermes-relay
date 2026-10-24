// The pairing QR payload and the code-for-token exchange (relay/internal/pairing, POST /api/pair).
import { ApiError, request, type Creds, type Fetch } from "./api";
import { isObject } from "./protocol";

// Crockford base32 as the relay prints it: XXXX-XXXX-XXXX, no I, L, O, U.
const CODE_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
const CODE_LEN = 12;
const TOKEN_RE = /^trs_[A-Za-z0-9_-]{43}$/;
// https only: the token travels over this. Host, optional port and path; no query, fragment or credentials.
const URL_RE = /^https:\/\/[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:\d{1,5})?(\/[A-Za-z0-9._~%/-]*)?$/;

/** Upper-cases a typed or scanned code and drops dashes and spaces; null if it can't be a code. */
export function normalizeCode(s: string): string | null {
  if (s.length > 64) return null;
  let out = "";
  for (const ch of s.toUpperCase()) {
    if (ch === "-" || ch === " ") continue;
    if (!CODE_ALPHABET.includes(ch)) return null;
    out += ch;
  }
  return out.length === CODE_LEN ? out : null;
}

/** Trims and checks a relay URL; returns it without a trailing slash, or null. */
export function normalizeUrl(s: string): string | null {
  const url = s.trim().replace(/\/+$/, "");
  return url.length <= 256 && URL_RE.test(url) ? url : null;
}

export type Pairing = { url: string; code: string };
export type ParseResult = ({ ok: true } & Pairing) | { ok: false; error: string };

export function parsePairingPayload(raw: string): ParseResult {
  let v: unknown;
  try {
    v = raw.length <= 1024 ? JSON.parse(raw) : undefined;
  } catch {
    v = undefined;
  }
  if (!isObject(v) || v.v !== 1) return { ok: false, error: "This QR code isn't a Trismegistus pairing code." };
  const url = typeof v.url === "string" ? normalizeUrl(v.url) : null;
  if (!url) return { ok: false, error: "The relay address in this code must be an https:// URL." };
  const code = typeof v.code === "string" ? normalizeCode(v.code) : null;
  if (!code) return { ok: false, error: "The pairing code in this QR code is missing or malformed." };
  return { ok: true, url, code };
}

/** Trades a one-time code for a device token. Throws ApiError (unauthorized = invalid, used or expired code). */
export async function redeem(url: string, code: string, fetchFn?: Fetch): Promise<Creds> {
  const body = await request(url, "/api/pair", { method: "POST", body: { code }, fetchFn });
  const device = isObject(body) && isObject(body.device) ? body.device.name : undefined;
  if (!isObject(body) || typeof body.token !== "string" || !TOKEN_RE.test(body.token) || typeof device !== "string") {
    throw new ApiError("hermes_error", "The relay sent an unexpected pairing response.");
  }
  return { url, token: body.token, device: device.slice(0, 64) };
}

/** What to tell the user when pairing fails. */
export function pairingErrorText(e: unknown): string {
  if (!(e instanceof ApiError)) return "Pairing failed.";
  switch (e.code) {
    case "unauthorized":
      return "That code is invalid, already used or expired. Run `pair` again for a new one.";
    case "rate_limited":
      return "Too many pairing attempts. Wait a minute and try again.";
    case "unreachable":
      return `${e.message} Check that the phone can reach the relay (LAN or tailnet).`;
    default:
      return e.message;
  }
}
