# Trismegistus: spec v1 (2026-10-23)

Trismegistus is an Android companion app for Hermes, plus the relay server it talks to.

## Goal

An Android app for talking to Hermes on optiplex from the phone: replies, live tool progress, approvals for risky
commands, and past conversations. A small relay server keeps conversations alive across flaky mobile
connections and keeps Hermes' API key off the phone.

## Decisions so far

- Android only. Expo (React Native, TypeScript, Bun). Expo Go for day-to-day work; APKs from EAS Build (free plan)
  or local builds on the Mac.
- Desktop (decided 2026-10-30): a native macOS menu bar companion in its own repo, `xMuramasa/trismegistus-mac`
  (Swift, SwiftUI), speaking this protocol against the same relay. No web, Electron or Tauri build of the Expo app.
  Linux is a Quickshell module in endfield-dotfiles later. The protocol stays owned here; that repo copies
  `protocol/` with a script and a staleness test.
- Relay: Go + WebSocket (chosen 2026-10-23 over Bun: one static binary, ~10-20 MB RAM). Not WebTransport: no React
  Native client, and it needs HTTP/3 end to end.
- The relay and the app share no code, so the WebSocket protocol is defined once in `protocol/schema.json` (JSON
  Schema) with example messages in `protocol/examples/`. Go tests and the app's tests must both accept every valid
  example and reject every invalid one.
- Hermes API server is on since 2026-10-23: `127.0.0.1:8642` on optiplex only, key `API_SERVER_KEY` in
  `~/.hermes/.env`. Never exposed directly.
- The relay is reachable on the LAN and tailnet only, through Caddy: `https://hermes.lab.x-muramasa.dev`.
- No text streaming: each answer appears whole when the turn finishes; tool progress is live (decided 2026-10-23;
  Hermes `streaming.enabled` stays off).
- Approvals stay in Hermes' smart mode: only commands the AI reviewer escalates reach the phone. The modal offers
  Once / This session / Deny; Hermes' `always` choice is hidden on purpose.
- Logs: the relay's stdout shows in Dozzle; Hermes' own logs are already there (`homelab/hermes-logs`).
- Private repo `xMuramasa/trismegistus` (`~/Projects/trismegistus` on optiplex, cloned to the Mac for Expo), direct
  commits to `main`.

## Flow

```
phone app --WSS--> Caddy (hermes.lab) --> relay 172.18.0.1:8650 --HTTP/SSE--> Hermes API 127.0.0.1:8642
```

- The phone only talks to the relay. The relay holds the Hermes key.
- One message from the phone = one Hermes run (`POST /v1/runs` with the session id). The relay reads the run's event
  stream, numbers every event, stores it, and sends it to every connected device.
- If the phone drops (signal, screen lock), the run keeps going. On reconnect the app sends the last event number it
  saw and gets the rest.

## Hermes API used

Checked on Hermes 0.22.2 (`/v1/capabilities`) and recorded in phase 0 (`probe/FINDINGS.md`, `probe/fixtures/`).

- Sessions: `GET /api/sessions` (`{data: [...], has_more}`; every source: api_server, telegram, cli, cron…),
  `POST /api/sessions {title?}` (titles must be unique, so the relay sends none), `GET/PATCH/DELETE
  /api/sessions/{id}`, `GET /api/sessions/{id}/messages` (`{data: [{id, role, content, tool_calls, tool_name,
  timestamp, ...}], pagination}`).
- Start a turn: `POST /v1/runs {input, session_id}` + `Idempotency-Key` → 202 `{run_id, status, replayed}`; the same
  key again returns the same run (`replayed: true`).
- Live events: `GET /v1/runs/{id}/events` (SSE, frames `data: {event, run_id, timestamp, seq, ...}`, no `event:` line,
  `: keepalive` every 10 s). Seen: `tool.started`, `tool.completed`, `subagent.start`, `subagent.complete`,
  `approval.request`, `approval.responded`, `reasoning.available` (carries the final answer text on this provider),
  `run.completed`, `run.cancelled`; documented but not seen: `run.failed`, `message.delta`, `message.interim`,
  `run.steered`. Hermes drops unread events after 5 minutes, so the relay reads them as they come.
- Run controls: `GET /v1/runs/{id}` (`status` incl. `waiting_for_approval` with an `approval` object),
  `POST /v1/runs/{id}/approval {request_id, choice}` (`execution_generation` may be null), `POST /v1/runs/{id}/stop`,
  `POST /v1/runs/{id}/steer {text}` (only while `running`, else 409).
- Later: `GET /api/model/options` for a model picker.

Limits:
- Hermes refuses turns in sessions another surface owns (Telegram, CLI…: 409). The relay only lets the phone write to
  `source: api_server` sessions; the others are listed and readable, read-only.
- Phone chats run on the main Hermes model and share the Claude usage limit with everything else.
- No text streaming (see Decisions): the answer arrives with `run.completed`.

## Relay (`relay/`)

- Go 1.27.3, one module in `relay/`. Dependencies kept to: `github.com/coder/websocket` (WebSocket),
  `modernc.org/sqlite` (pure-Go SQLite, no cgo), a QR-to-terminal package. Everything else is the standard library
  (`net/http`, `log/slog` JSON logs, `encoding/json`). Pinned in `go.mod`/`go.sum`.
- One binary, `trismegistus-relay`, with subcommands: `serve` (default), `pair --name <device>`,
  `devices list`, `devices revoke <name>`, `healthcheck` (for Docker; the image has no shell or curl).
- Storage: SQLite at `/data/relay.db` (named volume): devices, one-time pairing codes, the per-session event log.
- Container `trismegistus-relay`: multi-stage build (`golang:1.27.3-bookworm` → `gcr.io/distroless/static-debian12:
  nonroot`), `CGO_ENABLED=0`, ~25 MB image (6.4 MB compressed; measured 2026-10-24). `network_mode: host` so it can reach `127.0.0.1:8642`; listens on
  `172.18.0.1:8650` (the `proxy` network's gateway, where Caddy can reach it, like hindsight-ui). Non-root, read-only
  root filesystem, `cap_drop: ALL`, `mem_limit` 64m. The version is stamped with `--build-arg VERSION=...`.
- Config (env): `HERMES_URL` (default `http://127.0.0.1:8642`), `HERMES_API_KEY` (required), `RELAY_ADDR` (default
  `127.0.0.1:8650`), `PUBLIC_URL` (put in the pairing QR), `DATA_DIR` (default `/data`), `TRUSTED_PROXIES` (default
  `127.0.0.1/32,172.16.0.0/12`: peers whose `X-Forwarded-For` is used for rate limits), `LOG_LEVEL` (default `info`). The key goes in
  `homelab/trismegistus/.env` (mode 600), copied from `~/.hermes/.env` by an install script without printing it.
  Only that one variable: the rest of Hermes' `.env` never enters the container.

### Pairing and devices

- `docker exec -it trismegistus-relay trismegistus-relay pair --name pixel-8` prints a QR code in the terminal holding
  `{"v":1,"url":PUBLIC_URL,"code":...}` (a one-time code `XXXX-XXXX-XXXX`, Crockford base32, valid 10 minutes, single
  use, stored hashed), plus the code and expiry as text. An existing name fails unless `--replace`, which revokes the
  old device.
- The app scans it (camera works in Expo Go) and trades the code for a device token (`trs_` + base64url of 32 random
  bytes). The relay stores only its SHA-256, plus the name, creation time and last-seen time.
- `devices list` and `devices revoke <name>`. A revoked phone is disconnected at once (the server checks every 2 s,
  since the CLI runs as another process on the same database).
- The token never appears in chat, logs or the repo.

### REST (device token as Bearer)

- `GET /api/sessions`, `POST /api/sessions`, `GET /api/sessions/:id`, `GET /api/sessions/:id/messages`: passed to
  Hermes. `PATCH /api/sessions/:id` (rename) and `DELETE /api/sessions/:id` only for `source: api_server` sessions.
  Nothing else in Hermes' API is reachable through the relay (no artifacts, browser control, skills, admin).
- `POST /api/pair {code, name?}` → `{token, device}` (no auth; rate limited; the name comes from `pair --name`, a
  `name` in the body is ignored), `GET /healthz` (no auth, no details).
- `POST /api/sessions` ignores the body and creates an untitled session. Only `limit`, `offset`, `order`, `source`
  (list) and `limit`, `offset`, `order`, `before`, `after` (messages) query parameters are passed on. Errors are
  `{"error": {code, message}}` with the WebSocket error codes.

### WebSocket `/ws`

Messages are JSON objects with a `type` field, defined in `protocol/schema.json`.

- Phone → relay: `auth {token}` (must be the first message, within 5 s), `subscribe {session_id, after_seq?}`,
  `unsubscribe {session_id}`, `prompt {client_msg_id, session_id, text}`, `stop {run_id}`, `steer {run_id, text}`,
  `approve {run_id, request_id, choice: once|session|deny}`, `ping`.
- Relay → phone: `ready {device, server_version}`, `event {session_id, run_id, seq, kind, data, ts}` (`ts` in unix
  ms), `run {run_id, session_id, status}` (`running`, `waiting_for_approval`, `completed`, `failed`, `cancelled`; live
  only, and sent after a `subscribe` replay for the active run), `ack {client_msg_id, run_id, replayed}`,
  `error {code, message, ref?}`, `pong`. Error codes: `unauthorized`, `bad_request`, `too_large`, `rate_limited`,
  `busy`, `not_found`, `read_only`, `replay_gap`, `hermes_unavailable`, `hermes_error`, `internal`. Field caps:
  prompt/steer text ≤ 32768 characters, ids ≤ 128 characters of `[A-Za-z0-9_.:-]`. Per-kind `data` is in the schema.
- `stop`, `steer` and `approve` are accepted only from sockets subscribed to the run's session.
- `event.kind` is the relay's own small vocabulary, mapped from Hermes events: `tool.started`, `tool.completed`,
  `subagent.started`, `subagent.completed`, `approval.requested`, `approval.resolved`, `answer` (from `run.completed`
  output; `reasoning.available` is dropped as a duplicate), `run.completed`, `run.failed`, `run.cancelled`,
  `steer.queued` (sent when Hermes accepts a steer; Hermes' `run.steered` is dropped as a duplicate),
  `reconnected {reason: stream_lost|relay_restart}` (marks a gap: the relay lost the stream or restarted and is
  recovering), `unknown` (anything new; `data` passed through so the app can ignore it, emptied with `truncated`
  over 8 KiB). `message.delta` is dropped (streaming is off). `data` keeps only the fields the app needs (previews,
  durations, error flags, the approval command and description); no usage or cost numbers. Answers over 256 KiB are
  cut and marked `truncated`.
- Hermes' per-run `seq` is stored with each run; frames at or below it are skipped, so re-reading a stream after a
  drop or restart never duplicates events. A stream that ends without a terminal frame, or is silent for 45 s (no
  frame, no keepalive), is followed by `GET /v1/runs/{id}` polls with backoff (1 s doubling to 30 s, `Retry-After`
  respected): still running → re-attach; ended → the terminal events come from the status; unknown to Hermes or no
  contact for 2 hours → `run.failed`.
- `approval.requested` lists only `once`, `session`, `deny`; the relay refuses `always` from the phone.
- `seq` counts up per session across runs, from 1. Events are kept 24 hours for replay; a `subscribe` with an
  `after_seq` older than the buffer (or beyond the last seq, or more than 5000 events back) gets `error replay_gap`
  and the app reloads the session history over REST.
- `client_msg_id` (a UUID) becomes Hermes' `Idempotency-Key`, so a double tap or a resend after reconnecting can't
  start two turns.
- One run at a time per session. A prompt during a run gets `error busy`; the app offers to send it as a steer.
- Keepalive: the relay pings every 25 s and drops the socket after two missed pongs. Runs never depend on a socket
  being open.
- Relay restart: runs that were in progress are found again (`GET /v1/runs/{id}`) and re-attached or polled to their
  end; the gap is marked with a `reconnected` event and the answer comes from `run.completed` or the run status.

### Logging and limits

- JSON lines to stdout (`log/slog`): device name, session and run ids, event counts, durations, Hermes status codes.
  Never message text, approval commands or tokens.
- 64 KB per WebSocket message (bigger closes the socket, 1009), 20 prompts a minute per device, at most 5 devices and
  3 sockets per device (a 4th closes the oldest), 10 pairing attempts a minute per client IP (30 overall), 4
  requests waiting on Hermes per socket. A socket that falls 8192 messages behind is closed; the app resubscribes.
- WebSocket `Origin` is not checked: the app authenticates with a token in its first message, never a cookie.

## App (`apps/mobile`)

- Expo, TypeScript, Expo Router.
- SDK: Expo SDK 58 (React Native 0.88). Expo Go for SDK 58 is on the Play Store since 2026-10-19, so the app
  targets it directly. Only modules Expo Go bundles; no prebuild until phase 3.
- Libraries (phase 2a): Expo Router, NativeWind 4.2 + tailwindcss 3.4 (NativeWind 5 is an rc), zustand,
  react-native-marked for Markdown (pure JS, maintained), lucide-react-native icons, Cormorant Garamond / Inter /
  JetBrains Mono from `@expo-google-fonts`. The app's protocol decoder is hand-written (ajv generates code with
  `new Function`, which Hermes doesn't run); its tests check it against `protocol/examples/`, and the builders
  against the schema with ajv.
- Look: expressive, not stock. NativeWind (Tailwind for React Native) with shadcn-style components
  (react-native-reusables), light and dark themes, its own type and accent.
- Screens:
  1. **Pair:** scan the QR; the relay URL and token go in `expo-secure-store`.
  2. **Sessions:** list with title, last message and time; new chat; rename and delete.
  3. **Chat:** Markdown replies (shown when the turn finishes, with a working indicator meanwhile), code blocks with
     copy, collapsible tool rows ("terminal · 1.3 s"),
     subagent cards, Stop, steer while running, retry.
  4. **Approval modal:** a pop-up with the command preview and Once / This session / Deny.
  5. **Settings:** relay URL, device name, unpair, theme, connection status.
- One WebSocket. Reconnects with backoff on drops and when the app returns to the foreground, then resubscribes from
  the last `seq`. Sessions and recent messages are cached on the phone for offline reading.
- Tests: `bun test` for the protocol (against `protocol/examples/`) and the event reducer; a few component tests with
  `@testing-library/react-native`; typecheck and `expo-doctor`.

## Dev setups

- **Phone + Mac + optiplex relay:** the Expo dev server runs on the Mac; the app connects to
  `https://hermes.lab.x-muramasa.dev` over the tailnet.
- **Relay on the Mac:** `cd relay && go run ./cmd/trismegistus-relay` with `HERMES_URL` pointing at an SSH tunnel
  (`ssh -N -L 8642:127.0.0.1:8642 optiplex`) and a local `DATA_DIR`. The app's relay URL is a setting, so it can point at either.
- **APKs:** EAS Build free plan (a limited number of low-priority builds a month; check the account's billing page)
  or local builds on the Mac.

## Deploy (homelab)

- `~/Projects/homelab/trismegistus/docker-compose.yml` builds the relay from `~/Projects/trismegistus`.
- Caddy: `hermes.lab.x-muramasa.dev` → `reverse_proxy 172.18.0.1:8650` (WebSockets pass through); `docker restart
  caddy` after editing.
- Homepage tile. An Uptime Kuma monitor on `/healthz` is added by hand in its UI.
- No backup: `relay.db` holds only devices (re-pair) and a 24-hour event buffer.

## Security

- The Hermes key lives only in the relay's environment; phones get revocable tokens.
- LAN and tailnet only (no public DNS), TLS from Caddy.
- A paired, unlocked phone can approve commands that run on optiplex. No fingerprint check for now (one user); the
  approval modal is the only gate. `expo-local-authentication` can be added later.
- Push notifications (phase 3) go through Expo's push service and Firebase, so they carry no content: just
  "Reply ready" or "Approval needed".

## Phases

0. **Probe** (optiplex, no app): record real event streams for a plain answer, a tool call, an approval, a stop and a
   delegated subagent; check whether Telegram sessions can be read. Saved as fixtures. Costs a few small turns.
1. **Relay:** protocol schema and examples, Go relay, pairing CLI, tests against a fake Hermes that replays the
   phase 0 fixtures; deploy behind Caddy.
2. **App MVP in Expo Go**, in two steps:
   - **2a:** pair (QR + manual), session list (new, delete, read-only lock), chat (history, live tool rows,
     subagent cards, Markdown answers). One socket; reconnects only on a tap or when a chat opens.
   - **2b:** Stop, steer, reconnect with backoff and replay from the last `seq`, foreground resume, offline cache,
     settings screen, rename.
3. **Approvals and APK:** approval modal, development build, push notifications (needs a free
   Firebase project for FCM v1 and a development build; Expo Go can't receive pushes on Android).
4. **Later:** bot picker for other profiles (`/p/<profile>/`, each with its own key), a news tab from news-reader,
   cron jobs (needs Hermes' jobs API, off now), model picker, voice.

Each phase is done when its tests pass, it's committed and pushed, and (from phase 2) it's checked on the phone.

## Answered (2026-10-23)

- Address: `hermes.lab.x-muramasa.dev`.
- Pairing: QR printed by a command on optiplex (one user for now).
- Approvals: a modal in the app, no fingerprint.
- Code: `~/Projects/trismegistus` on optiplex; the Mac gets a clone.
- Name: Trismegistus (app, repo, relay container).
