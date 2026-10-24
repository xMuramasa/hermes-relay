# Trismegistus: spec v1 (2026-10-23)

Trismegistus is an Android companion app for Hermes, plus the relay server it talks to.

## Goal

An Android app for talking to Hermes on optiplex from the phone: streamed replies, live tool progress, approvals for
risky commands, and past conversations. A small relay server keeps conversations alive across flaky mobile
connections and keeps Hermes' API key off the phone.

## Decisions so far

- Android only. Expo (React Native, TypeScript, Bun). Expo Go for day-to-day work; APKs from EAS Build (free plan)
  or local builds on the Mac.
- Relay: Bun + WebSocket. Not WebTransport: no React Native client, and it needs HTTP/3 end to end.
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

Checked against `/v1/capabilities` on Hermes 0.22.2.

- Sessions: `GET/POST /api/sessions`, `GET/PATCH/DELETE /api/sessions/{id}`, `GET /api/sessions/{id}/messages`.
- Start a turn: `POST /v1/runs` `{input, session_id}` with an `Idempotency-Key` header (a retry never starts a second
  turn).
- Live events: `GET /v1/runs/{id}/events` (SSE): `message.delta`, `message.interim`, `tool.started`,
  `tool.completed`, `subagent.start`/`subagent.complete`, `approval.request`, then `run.completed`, `run.failed` or
  `run.cancelled`. Hermes drops unread events after 5 minutes, so the relay reads them as they come. Phase 0 results
  (what actually arrives on this server) are in `probe/FINDINGS.md`.
- Run controls: `GET /v1/runs/{id}` (status and `pending_controls`), `POST /v1/runs/{id}/approval`
  `{request_id, execution_generation, choice}`, `/clarify`, `/stop`, `/steer`.
- Later: `GET /api/model/options` for a model picker.

Limits:
- API sessions are separate from Telegram and CLI sessions. The API refuses to run turns in a Telegram chat (409),
  but can list and read them (phase 0), so the app can show them read-only.
- The exact event payloads are only partly documented. Phase 0 records real ones as test fixtures.
- Phone chats run on the main Hermes model and share the Claude usage limit with everything else.

## Relay (`apps/relay`)

- Bun + TypeScript, `bun:sqlite` at `/data/relay.db` (named volume). Container `trismegistus-relay`.
- Container: `network_mode: host` so it can reach `127.0.0.1:8642`; listens on `172.18.0.1:8650` (the `proxy`
  network's gateway, where Caddy can reach it, like hindsight-ui). Non-root, read-only root filesystem, `mem_limit`
  128m.
- Config (env): `HERMES_URL` (default `http://127.0.0.1:8642`), `HERMES_API_KEY`, `RELAY_HOST`, `RELAY_PORT`,
  `PUBLIC_URL`. The key goes in `homelab/trismegistus/.env` (mode 600), copied from `~/.hermes/.env` by an install
  script without printing it. Only that one variable: the rest of Hermes' `.env` never enters the container.

### Pairing and devices

- `docker exec -it trismegistus-relay bun run pair --name pixel-8` prints a QR code in the terminal holding the relay URL
  and a one-time code, valid for 10 minutes.
- The app scans it (camera works in Expo Go) and trades the code for a device token (32 random bytes). The relay
  stores only its SHA-256, plus the name, creation time and last-seen time.
- `bun run devices list` and `bun run devices revoke <name>`. A revoked phone is disconnected at once.
- The token never appears in chat, logs or the repo.

### REST (device token as Bearer)

- `GET/POST /api/sessions`, `GET /api/sessions/:id/messages`, `PATCH/DELETE /api/sessions/:id`: passed to Hermes from
  an allowlist. Nothing else in Hermes' API is reachable through the relay (no artifacts, browser control or admin).
- `POST /api/pair` (one-time code → device token), `GET /healthz` (no auth, no details).

### WebSocket `/ws`

Messages are JSON, validated with zod schemas in `packages/protocol`, shared with the app.

- Phone → relay: `auth {token}` (must be the first message, within 5 s), `subscribe {session_id, after_seq?}`,
  `prompt {client_msg_id, session_id, text}`, `stop {run_id}`, `steer {run_id, text}`,
  `approve {run_id, request_id, generation, choice}`, `clarify {run_id, request_id, generation, answer}`, `ping`.
- Relay → phone: `ready {device, server_version}`, `event {session_id, run_id, seq, type, data, ts}`,
  `run {run_id, session_id, status}`, `ack {client_msg_id, run_id}`, `error {code, message}`, `pong`.
- `seq` counts up per session. Events are kept 24 hours for replay.
- `client_msg_id` becomes Hermes' `Idempotency-Key`, so a double tap or a resend after reconnecting can't start two
  turns.
- One run at a time per session. A prompt during a run gets `error busy`; the app offers to send it as a steer.
- Keepalive: ping every 25 s, drop the socket after two missed. Runs never depend on a socket being open.
- Relay restart: runs still marked running are re-attached (or polled to their end); the gap shows as a
  "reconnected" marker and the final answer is taken from the session history.

### Logging and limits

- JSON lines to stdout: device name, session and run ids, event counts, durations. Never message text, never tokens.
- 64 KB per WebSocket message, 20 prompts a minute per device, at most 5 devices and 3 sockets per device.

## App (`apps/mobile`)

- Expo, TypeScript, Expo Router.
- SDK: Expo SDK 58 is out (React Native 0.88), but Expo Go for SDK 58 isn't on the Play Store yet. Start on the
  newest SDK the store's Expo Go runs (checked on day 1) and upgrade when it catches up.
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
- Tests: `bun test` for the protocol and the event reducer (on phase 0 fixtures); a few component tests with
  `@testing-library/react-native`; typecheck and `expo-doctor`.

## Dev setups

- **Phone + Mac + optiplex relay:** the Expo dev server runs on the Mac; the app connects to
  `https://hermes.lab.x-muramasa.dev` over the tailnet.
- **Relay on the Mac:** `bun run relay:dev` with `HERMES_URL` pointing at an SSH tunnel
  (`ssh -N -L 8642:127.0.0.1:8642 optiplex`). The app's relay URL is a setting, so it can point at either.
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
1. **Relay:** protocol, relay, pairing CLI, tests against a fake Hermes built from the fixtures; deploy behind Caddy.
2. **App MVP in Expo Go:** pair, sessions, streamed chat, stop, reconnect and replay.
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
