# Trismegistus: spec v1 (2026-10-23)

Trismegistus is the relay server and WebSocket protocol behind Hermes' companion apps: the native macOS app
(`xMuramasa/trismegistus-mac`) and a native Android app (planned, its own repo). Until 2026-11-02 this repo also held
the Android app as an Expo project (`apps/mobile`); it was removed for a native build and is kept at the tag
`expo-archive`.

## Goal

Apps for talking to Hermes on optiplex from the phone and the Mac: replies, live tool progress, approvals for risky
commands, and past conversations. A small relay server keeps conversations alive across flaky mobile
connections and keeps Hermes' API key off the devices.

## Decisions so far

- Android (decided 2026-11-02): a native app (Kotlin, Jetpack Compose) in its own repo, built in Docker on optiplex
  and sideloaded, its usability mapped onto the Mac app's. It replaces the Expo app (`apps/mobile`, phase 2a, built
  2026-10-24 and never checked on the phone), archived at the tag `expo-archive`. See Android app below.
- Desktop (decided 2026-10-30): a native macOS menu bar companion in its own repo, `xMuramasa/trismegistus-mac`
  (Swift, SwiftUI), speaking this protocol against the same relay. No web, Electron or Tauri build.
  Linux is a Quickshell module in endfield-dotfiles later. The protocol stays owned here; that repo copies
  `protocol/` with a script and a staleness test.
- Relay: Go + WebSocket (chosen 2026-10-23 over Bun: one static binary, ~10-20 MB RAM). Not WebTransport: no React
  Native client, and it needs HTTP/3 end to end.
- The relay and its apps share no code, so the WebSocket protocol is defined once in `protocol/schema.json` (JSON
  Schema) with example messages in `protocol/examples/`. The relay's tests and each app's tests must accept every
  valid example and reject every invalid one (an app may leave length and pattern limits to the relay).
- Hermes API server is on since 2026-10-23: `127.0.0.1:8642` on optiplex only, key `API_SERVER_KEY` in
  `~/.hermes/.env`. Never exposed directly.
- The relay is reachable on the LAN and tailnet only, through Caddy: `https://hermes.lab.x-muramasa.dev`.
- No text streaming: each answer appears whole when the turn finishes; tool progress is live (decided 2026-10-23;
  Hermes `streaming.enabled` stays off).
- Approvals stay in Hermes' smart mode: only commands the AI reviewer escalates reach the phone. The modal offers
  Once / This session / Deny; Hermes' `always` choice is hidden on purpose.
- Logs: the relay's stdout shows in Dozzle; Hermes' own logs are already there (`homelab/hermes-logs`).
- Private repo `xMuramasa/trismegistus` (`~/Projects/trismegistus` on optiplex), direct
  commits to `main`.

## Flow

```
phone / Mac app --WSS--> Caddy (hermes.lab) --> relay 172.18.0.1:8650 --HTTP/SSE--> Hermes API 127.0.0.1:8642
```

- The phone only talks to the relay. The relay holds the Hermes key.
- One message from the phone = one Hermes run (`POST /v1/runs` with the session id). The relay reads the run's event
  stream, numbers every event, stores it, and sends it to every connected device.
- If the phone drops (signal, screen lock), the run keeps going. On reconnect the app sends the last event number it
  saw and gets the rest.

## Hermes API used

Checked on Hermes 0.22.2 (`/v1/capabilities`) and recorded in phase 0 (`probe/FINDINGS.md`, `probe/fixtures/`).

- Sessions: `GET /api/sessions` (`{data: [...], has_more}`; every source: api_server, telegram, cli, cron…),
  `POST /api/sessions {title?}` (titles must be unique, so the relay sends none), `GET /api/sessions/{id}`,
  `PATCH /api/sessions/{id}` (accepts `title`, `pinned`, `archived`, `hidden`, `unread`, `end_reason`; unknown keys
  are a 400; `pinned` is Hermes' durable keep flag, exempt from auto-prune, and the list carries it),
  `DELETE /api/sessions/{id}` (hard delete; `{object: "hermes.session.deleted", id, deleted}`, or 409
  `session_active_turn` while a turn is writing), `GET /api/sessions/{id}/messages` (`{data: [{id, role, content,
  tool_calls, tool_name, timestamp, ...}], pagination}`).
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
- Models: `GET /api/model/options?include_unconfigured=false` (`{model, provider, providers: [{slug, name, models:
  [ids], capabilities: {<id>: {fast, reasoning}}, authenticated, ...}]}`; ids may contain `[1m]`) and `POST
  /api/sessions/{id}/model {provider, model}` (a persisted per-session lock; checked 2026-10-31 in
  `gateway/platforms/api_server.py`: 404 unknown session, 400 no model/provider, 409 `model_lock_unavailable` for a
  provider without a model, otherwise 200 `{object: "hermes.session.model_lock", session_id, runtime}`; Hermes does
  not check the model against its catalog, the relay does). Only `/api/sessions/{id}/chat` reads that lock;
  `POST /v1/runs` ignores it but honors `provider` + `model` in its body, so the relay sends the chosen pair with
  every run.

Limits:
- Hermes refuses turns in sessions another surface owns (Telegram, CLI…: 409). The relay only lets the phone send turns to
  `source: api_server` sessions; the others are listed and readable, and can be renamed, pinned and tagged (see REST).
- Phone chats share the Claude usage limit with everything else (unless a chat's chosen model runs elsewhere, e.g.
  GitHub Copilot). A chat's model choice (REST) is stored in the relay and as Hermes' session lock, and sent as
  `provider` + `model` with each `POST /v1/runs` (which ignores the lock). The `Idempotency-Key` covers the body, so a
  resend after the model changed may be refused rather than replayed.
- No text streaming (see Decisions): the answer arrives with `run.completed`.

## Relay (`relay/`)

- Go 1.27.3, one module in `relay/`. Dependencies kept to: `github.com/coder/websocket` (WebSocket),
  `modernc.org/sqlite` (pure-Go SQLite, no cgo), a QR-to-terminal package. Everything else is the standard library
  (`net/http`, `log/slog` JSON logs, `encoding/json`). Pinned in `go.mod`/`go.sum`.
- One binary, `trismegistus-relay`, with subcommands: `serve` (default), `pair --name <device>
  [--role manager|viewer]`, `devices list`, `devices set-role <name> <role>`, `devices revoke <name>`, `healthcheck` (for Docker; the image has no shell or curl).
- Storage: SQLite at `/data/relay.db` (named volume): devices, one-time pairing codes, the per-session event log,
  session tags, handoff links, per-session model choices and image attachments (files in `/data/attachments/`).
- Container `trismegistus-relay`: multi-stage build (`golang:1.27.3-bookworm` → `gcr.io/distroless/static-debian12:
  nonroot`), `CGO_ENABLED=0`, ~25 MB image (6.4 MB compressed; measured 2026-10-24). `network_mode: host` so it can reach `127.0.0.1:8642`; listens on
  `172.18.0.1:8650` (the `proxy` network's gateway, where Caddy can reach it, like hindsight-ui). Non-root, read-only
  root filesystem, `cap_drop: ALL`, `mem_limit` 64m. The version is stamped with `--build-arg VERSION=...`.
- Config (env): `HERMES_URL` (default `http://127.0.0.1:8642`), `HERMES_API_KEY` (required), `RELAY_ADDR` (default
  `127.0.0.1:8650`), `PUBLIC_URL` (put in the pairing QR), `DATA_DIR` (default `/data`), `TRUSTED_PROXIES` (default
  `127.0.0.1/32,172.16.0.0/12`: peers whose `X-Forwarded-For` is used for rate limits), `LOG_LEVEL` (default `info`). The key goes in
  `homelab/trismegistus/.env` (mode 600), copied from `~/.hermes/.env` by an install script without printing it.
  Only that one variable: the rest of Hermes' `.env` never enters the container. GitHub (optional, see GitHub
  below): `GITHUB_TOKEN` (in the same `.env`, added by Martin), `GITHUB_OWNER` (`xMuramasa`), `GITHUB_PROJECT` (the
  Work board's node id, `PVT_…`). No token: the GitHub endpoints answer `not_configured`; a token without a valid
  owner or a `PVT_` project stops `serve` at start.

### Pairing and devices

- `docker exec -it trismegistus-relay trismegistus-relay pair --name pixel-8` prints a QR code in the terminal holding
  `{"v":1,"url":PUBLIC_URL,"code":...}` (a one-time code `XXXX-XXXX-XXXX`, Crockford base32, valid 10 minutes, single
  use, stored hashed), plus the code and expiry as text. An existing name fails unless `--replace`, which revokes the
  old device.
- The app scans it (the Mac app takes the pasted code) and trades the code for a device token (`trs_` + base64url of 32 random
  bytes). The relay stores only its SHA-256, plus the name, creation time and last-seen time.
- `devices list` and `devices revoke <name>`. A revoked phone is disconnected at once (the server checks every 2 s,
  since the CLI runs as another process on the same database).
- The token never appears in chat, logs or the repo.
- Every device has a role: `viewer` (the default, and what devices paired before roles became) or `manager`.
  `pair --name mac --role manager` pairs a manager; `devices set-role <name> manager|viewer` changes a paired
  device's role, effective on its next request (no re-pairing, no restart); `devices list` shows it. Chat, prompts,
  stop/steer, approvals, models, handoff links and session list/create/rename/pin/delete/tags are open to both roles. Management endpoints (cron jobs,
  GitHub actions, stopping background agents; for the Mac app) are manager-only and answer viewers `403
  {"error": {"code": "forbidden", ...}}`. Roles are REST-only: the WebSocket messages (including `ready`) and
  `protocol/schema.json` don't carry them, since the Mac app decodes `ready` strictly.

### REST (device token as Bearer)

- `GET /api/sessions`, `POST /api/sessions`, `GET /api/sessions/:id`, `GET /api/sessions/:id/messages`: passed to
  Hermes. Nothing else in Hermes' API is reachable through the relay (no artifacts, browser control, skills, admin).
- `PATCH /api/sessions/:id {title?, pinned?}` (rename and/or pin, at least one): every source. The title is trimmed,
  1-200 characters; `pinned` is a bool. Only these two keys are sent to Hermes; any other key is `bad_request`.
- `DELETE /api/sessions/:id`: only sources `api_server`, `cron` and `cli` (an allowlist, so new messaging platforms
  stay protected; Telegram is the main chat's memory); others get `read_only`. While a run is writing to the session
  (the relay's own, or Hermes' 409 `session_active_turn` for another surface's turn) it answers `busy` (HTTP 409)
  "a run is still writing to this session". A successful delete also drops the session's tags.
- Tags are the relay's own (Hermes has none), so they follow every device. `GET /api/tags` →
  `{"sessions": {"<id>": ["tag", ...]}}` (sessions without tags are left out). `PUT /api/sessions/:id/tags
  {"tags": [...]}` replaces the set (`[]` clears it) and answers `{"tags": [...]}` as stored; the session must exist in
  Hermes (any source). A tag is trimmed, 1-32 characters, no control characters; duplicates are compared
  case-insensitively and the first spelling is kept; at most 10 per session (after dedupe). Ids of sessions deleted
  elsewhere (e.g. the nightly cron cleanup) may linger in `GET /api/tags`; apps ignore ids they don't list.
- Models. `GET /api/models` → `{"current": {"provider", "model"}, "providers": [{"slug", "name", "models": [{"id",
  "reasoning"}]}]}`: Hermes' connected providers only (`include_unconfigured=false`), cached 5 minutes (Hermes
  errors are not cached and map as usual). `PUT /api/sessions/:id/model {"provider", "model"}`: the pair must be in
  that list (else `bad_request`) and the session a relay chat (`api_server`; others get `read_only`, since the lock
  would change another surface's conversation); the relay forwards Hermes' session lock, stores the choice and
  answers `{"provider", "model"}`. `GET /api/session-models` → `{"sessions": {"<id>": {"provider", "model"}}}`;
  sessions left out use `current`.
- Handoff links: when a conversation grows too large the app starts a new one from a digest and shows both as one
  thread. The link lives in the relay, not Hermes (Hermes' own compression children redirect the old id to the
  child, which would hide the old messages). `PUT /api/sessions/:id/continues {"from": "<older id>"}` records that
  `:id` continues the older conversation (replacing any earlier link of `:id`); both must exist in Hermes (any
  source). Chains stay straight: a conversation continues at most one older one, each older one is continued at most
  once, no self-links or loops, at most 32 links per chain; violations are `bad_request`. `{"from": null}` removes
  the link (no existence check). Answers `{"from": ...}` as stored. `GET /api/links` → `{"continues": {"<newer>":
  "<older>"}}`. Deleting a session through the relay drops the links on both sides of it (leaving two shorter
  chains) and its model choice; ids deleted elsewhere may linger, as with tags.
- Image attachments (#25; WebSocket messages are capped at 64 KiB, so images have their own upload path; Hermes keeps
  only the text plus a `[screenshot]` line, never the image, so the relay stores them):
  - `POST /api/attachments`, `manager` only (`forbidden` for viewers). Body: the image bytes, `Content-Type`
    `image/png`, `image/jpeg` or `image/gif`, at most 8 MiB (else `too_large`, 413). The bytes must decode as that
    type (`image.DecodeConfig`; else `bad_request`). Answers 201, or 200 when the same bytes were already there:
    `{"id": "<sha256 hex, 64>", "mime", "width", "height", "bytes"}`. Uploads no prompt used within 1 h are deleted
    (by the hourly cleanup, so after 1-2 h; uploading the same bytes again restarts the hour).
  - `GET /api/attachments/:id`, any device: the bytes with their `Content-Type`, `Cache-Control: private,
    max-age=31536000, immutable`, `ETag: "<id>"` (`If-None-Match` → 304); `not_found` otherwise.
  - A `prompt` may carry `attachments` (see WebSocket). The relay sends Hermes `POST /v1/runs` with `"input": [{"role":
    "user", "content": [{"type": "text", "text"}, {"type": "image_url", "image_url": {"url": "data:<mime>;base64,…"}},
    …]}]` (no text part when the text is empty); the files are read and the data URLs built only for that request,
    never logged.
  - `GET /api/sessions/:id/messages` adds `"attachments": [{"id", "mime", "width", "height"}]` to the user messages that
    relay runs with images produced: the session's runs with images in `created_at` order, each matched to the first
    unmatched user message of the page whose `timestamp` is within [created_at − 5 s, created_at + 120 s]. Other fields
    are passed on unchanged. Matching is per page, so a page that starts just after an image run's message may show
    that run's images on a user message of the next 2 minutes.
  - Storage: files `/data/attachments/<id>`, tables `attachments` and `run_attachments`. Runs with images are kept past
    the 24 h run cleanup (the history needs their time). Deleting a session through the relay drops its links and the
    files no other run uses.
- A conversation's images on the host (#26): Hermes' `MEDIA:<path>` lines in answers (`/v1/runs` doesn't resolve
  them) and Telegram's `[Image attached at: <path>]` notes (the images live in `~/.hermes/cache/images/`).
  `GET /api/sessions/:id/media?path=<absolute path>`, any device: served only if the path occurs in that session's
  messages from Hermes, as an assistant message's `MEDIA:` tag or a user message's image note; anything else is
  `not_found`, which never says whether the file exists. The relay reads the session's messages (500 a page, up to
  10000) and caches its paths 60 s. The bytes come from the agents helper's `GET /media?path=` (see Background agents;
  the helper applies its own path rules): 200 with the image's `Content-Type` and `Cache-Control: private,
  max-age=3600`; the helper's 404 is `not_found`; the helper not set up or not running is 503 `hermes_unavailable`.
- `POST /api/pair {code, name?}` → `{token, device}` (no auth; rate limited; the name comes from `pair --name`, a
  `name` in the body is ignored), `GET /healthz` (no auth, no details).
- `GET /api/me` → `{device, role}` (the device's name and `manager` or `viewer`): how an app learns its role.
- `POST /api/sessions` ignores the body and creates an untitled session. Only `limit`, `offset`, `order`, `source`
  (list) and `limit`, `offset`, `order`, `before`, `after` (messages) query parameters are passed on. Errors are
  `{"error": {code, message}}` with the WebSocket error codes, plus the REST-only `forbidden` (HTTP 403: the
  endpoint needs a `manager` device).

### Cron jobs (REST)

Hermes' cron jobs API (`/api/jobs`, checked 2026-11-01 in `gateway/platforms/api_server.py` and `cron/jobs.py`)
behind the relay, for the Mac app (create, edit, pause, resume, run now, delete); the phone only views. Job ids are
Hermes' 12 lowercase hex characters; any other `:id` is `not_found` without asking Hermes.

- Every device: `GET /api/jobs` → `{"jobs": [job, ...]}` (paused jobs included: the relay asks Hermes with
  `include_disabled=true`), `GET /api/jobs/:id` → `{"job": job}`, `GET /api/jobs/:id/runs` → `{"runs": [{"id",
  "started_at", "title"}, ...]}`: the job's run sessions (`cron_<job id>_<YYYYMMDD_HHMMSS>`, openable like any session),
  newest first, at most 50. Hermes' session list has no id filter, so the relay pages its 1000 most recently active
  `source=cron` sessions; `started_at` (unix seconds) and `title` are as Hermes has them, possibly null.
- `manager` only (`forbidden` for viewers): `POST /api/jobs` → `{"job"}`, `PATCH /api/jobs/:id` → `{"job"}`,
  `DELETE /api/jobs/:id` → `{"ok": true}`, `POST /api/jobs/:id/pause|resume|run` → `{"job"}` (any body is ignored;
  Hermes' one-off extra prompt for `run` is not offered).
- Write allowlist: exactly the fields Hermes' API accepts, so it replaces the issue's longer list (#20). Create:
  `name` (required, ≤ 200), `schedule` (required: cron expression, `every 2h`, `30m`, ISO time...), `prompt` (≤ 5000,
  injection-scanned by Hermes), `deliver` (default `local`), `skills` (list), `repeat` (positive int; absent =
  forever, 1 for one-shots), `paused` (bool), `paused_reason` (needs `paused`). Update: `name`, `schedule`, `prompt`,
  `deliver`, `skills`, `skill`, `repeat` (0 = forever), `enabled`. Any other key, or a wrong type, is `bad_request`
  before Hermes is asked; only the keys sent are forwarded. Bodies ≤ 16 KiB.
- Read-only (shown as Hermes has them, never written from the app): `model`, `provider`, `enabled_toolsets`,
  `script`, `workdir`, `context_from`, `failure_deliver`. So the app can't add scripts or set a working directory.
- A job as the relay answers it, keys from `cron/jobs.py`, others (`origin`, monitor settings, delivery bookkeeping,
  `latest_execution`) dropped: `id`, `name`, `prompt`, `schedule` (`{kind: once|interval|cron, expr?, minutes?,
  run_at?, display}`), `schedule_display`, `deliver`, `failure_deliver` (absent when unset), `skills`, `repeat`
  (`{times: n|null, completed}`), `enabled`, `state` (`scheduled`, `paused`, `completed`, `error`), `paused_at`,
  `paused_reason`, `model`, `provider`, `enabled_toolsets`, `script`, `workdir`, `context_from`, `created_at`,
  `next_run_at`, `last_run_at`, `last_status`, `last_error` (times are ISO 8601 strings or null).
- Errors: Hermes' 400 → `bad_request` with Hermes' message (e.g. "Name must be ≤ 200 characters", "No valid fields
  to update", the prompt scanner's refusal); Hermes' 500 with a message (an update with a bad schedule, running a
  finished job) → `hermes_error` with that message; 404 → `not_found`; 501 (Hermes without cron) →
  `hermes_unavailable`; 424 → `hermes_error` "the job was saved but its schedule was not registered: ..." (reload the
  list); unreachable → `hermes_unavailable`.
- Logs: each write logs `job <create|update|delete|pause|resume|run>` with the job id, device and outcome (`ok` or
  the error code); never prompts, names or Hermes' messages.

### GitHub (REST, for the Mac app; added 2026-11-01)

The relay talks to GitHub so no GitHub token lives on a device. It uses GitHub's GraphQL API only
(`https://api.github.com/graphql`, a config default) with fixed queries: requests fill in a repo name, an issue number,
an option and comment text, never a query. Only `GITHUB_OWNER`'s repos and the `GITHUB_PROJECT` board are reachable.
The board's `Status` and `Priority` single-select fields are found by name and their ids cached (10 minutes); nothing
is hard-coded. Reads are cached 30 s (errors are not cached); any write drops the cache.

- `GET /api/github/board` → `{"status_options": [...], "priority_options": [...], "items": [{"repo": "owner/name",
  "number", "title", "state": "open"|"closed", "status", "priority", "labels": [...], "updated_at", "url"}]}`. Options
  are names in board order; an unset status or priority is `""`. Only issues of the owner's repos (pull requests,
  drafts and other owners' issues are left out); up to 1000 items.
- `GET /api/github/repos` → `{"repos": ["owner/name", ...]}`: the owner's repos with issues on the board, sorted.
- `GET /api/github/repos/{owner}/{repo}/issues?state=open|closed` (default `open`) → `{"repo", "state", "total",
  "issues": [{"number", "title", "state", "labels", "comments", "updated_at", "url"}]}`: the 100 most recently
  updated.
- `GET /api/github/repos/{owner}/{repo}/issues/{number}` → `{"repo", "number", "title", "body", "state", "author",
  "labels", "created_at", "updated_at", "url", "on_board", "status", "priority", "comments_total", "comments":
  [{"author", "body", "created_at", "url"}]}` (the last 100 comments, oldest first). Issues not on the board can be
  read (`on_board: false`).
- Manager only (viewers get `forbidden`):
  - `POST .../issues/{number}/comments {"body"}` (non-empty; request ≤ 16 KiB) → 201 `{"author", "body",
    "created_at", "url"}`.
  - `PATCH .../issues/{number} {"state": "open"|"closed"}` (reopen or close) → `{"state"}`.
  - `PUT .../issues/{number}/board {"status"?, "priority"?}` (option names from `GET /api/github/board`, at least
    one) → `{"status", "priority"}`. An issue not on the board is added to it first.
- `{owner}` must be `GITHUB_OWNER` (case-insensitive), else `forbidden`; pull request numbers are `not_found`.
- Errors are `{"error": {code, message, retry_at?}}`, codes REST-only: `not_configured` (503: no `GITHUB_TOKEN`, or
  GitHub refused it), `rate_limited` (429, `retry_at` RFC 3339 and `Retry-After`, from GitHub's rate-limit headers),
  `not_found` (404), `forbidden` (403: a viewer, another owner, or the token lacks a scope), `bad_request` (400:
  unknown option, a board without that field), `github_error` (502: GitHub down or an unexpected answer).
- Logs carry the route, repo name, issue number, device and GitHub's HTTP status; never issue or comment text or the
  token. Starting a Hermes conversation from an issue is the app's job (create a session, send a prompt).

### Background agents (REST, for the Mac app; added 2026-11-01)

Watch background work on optiplex: Hermes' background processes and the Claude Code delegate runs among them, with
progress, a transcript and a stop button. The relay's container is read-only and can't see host PIDs, so a host
helper does the work and the relay passes its JSON on.

- **Host helper** `trismegistus-agents` (`agents/` in this repo, its own Go module, standard library only): a
  systemd **user** service (`agents/trismegistus-agents.service`, install steps in `agents/README.md`) serving HTTP
  on a Unix socket: the unit uses `~/.local/state/trismegistus-agents/agents.sock` (`-socket`; the built-in default
  is under `$XDG_RUNTIME_DIR`, which is emptied at boot and so a bad bind-mount source; directory 0755, socket 0666
  so the container's uid 65532 can connect; the home directory is 0750). The socket's directory is bind-mounted into the relay container and
  `AGENTS_SOCKET` (config, optional, absolute path) points at it; unset, or the helper not running, the endpoints
  answer `503 {"error": {"code": "not_configured", ...}}`.
- Sources (checked 2026-11-01 in `tools/process_registry.py` and `~/.hermes/processes.json`): Hermes' process list,
  a JSON list of its **running** background processes (`session_id` `proc_…`, `command`, `pid`, `host_start_time`
  (`/proc/<pid>/stat` field 22, against pid reuse), `systemd_unit` (a user scope holding the process tree), `cwd`,
  `started_at`, `parent_session_id`, …; numbers may be JSON numbers or strings). Claude Code delegate runs are the
  entries whose command is `bash <runs dir>/<name>-launch.sh` (`-runs`, comma-separated, default `~/.local/state/hermes-runs,~/.hermes/cache/scratch/tmac/wt`);
  next to the script are `<name>.stream.jsonl` (Claude Code `stream-json`) and `<name>-progress.md`. Finished runs
  leave the process list but their files stay: those written in the last 7 days are listed as finished. Runs started
  by `dispatch` (a transient `dispatch-<name>.service` user unit, not in the process list) add `<name>.run.json`; it
  gives the agent `repo`, `issue`, `issue_title` and `model`, and the run is alive while its unit is active.
- Not covered: **Hermes subagents** (`delegate_task`) live only inside the gateway process
  (`tools/delegate_tool_registry.py`) and its API server has no endpoint for them, so they can't be listed without
  changing Hermes. Other background processes have **no transcript**: Hermes keeps their output in its own memory
  (no log file on the host).
- The helper reads only the process list, `/proc/<pid>/stat`, and the stream and progress files of the delegate runs
  it lists; never environment variables (`/proc/<pid>/environ`), command lines from `/proc`, or other files. It never
  answers `session_key`, task ids or `watcher_*` values.
- Every device: `GET /api/agents` → `{"agents": [agent, ...]}`, running first, then newest first. An agent: `id`
  (`proc_…` for a process, `run_<name>` for a delegate run, alive or finished), `kind` (`process` or `delegate`),
  `title` (the run's name, or the command's first 80 characters on one line), `command`, `cwd` (a finished run's from
  its init line), `started_at` (unix seconds; a finished run's is its end minus `duration_ms`), `ended_at` (finished
  runs: when the transcript was last written), `alive` (the pid exists, isn't a zombie and its start time still
  matches), `session_id` (the Hermes conversation that started it, when Hermes recorded one), `transcript` (bool),
  and for delegate runs `progress` (the last line of the progress file, ≤ 300 characters), `outcome` (the result
  line's `subtype`: `success`, `error_max_turns`, …; absent while running or when killed), `num_turns`,
  `duration_ms`. Empty fields are left out.
- Every device: `GET /api/agents/:id/transcript?after=N` → `{"steps": [{"line", "kind", "text", "error"?}],
  "next", "alive"}`: readable steps from stream-json lines after line `N` (default 0; complete lines only, at most
  200 steps per answer: ask again with `after=next`). Kinds: `text` (assistant text), `tool` ("Bash: go test ./..."
  : the tool's name and a one-line summary of its main argument, ≤ 160 characters), `tool_result` (≤ 400 characters,
  `error` when the tool failed), `result` (the final answer, ≤ 8000 characters). Thinking and system lines are
  skipped. Processes without a transcript answer `{"steps": [], "next": N}`. `N` must be digits (else
  `bad_request`); an unknown or expired id is `not_found`.
- `manager` only (`forbidden` for viewers): `POST /api/agents/:id/stop` → 202 `{"stopping": true}`, only for a
  listed, alive process whose start time still matches (else `not_found`). The helper runs `systemctl --user stop
  <unit>` when Hermes made a scope (it takes the whole tree, the launch script's `claude` too), otherwise SIGTERM to
  the process group, SIGKILL after 10 s. It answers at once; the list shows `alive: false` when it's done. Each stop
  is logged with the id, device and outcome.
- Ids are checked by the relay (`proc_` + up to 64 letters/digits, or `run_` + a name of letters, digits, `.`, `_`,
  `-`) before the helper is asked; any other `:id` is `not_found`.
- The helper also serves chat images for `GET /api/sessions/:id/media` (#26): `GET /media?path=<absolute path>`, only
  a clean absolute path that, as asked and with symlinks resolved, lies under the user's home with no component
  starting with `.` except under `~/.hermes/cache/`; a regular file with extension png, jpg, jpeg, gif or webp whose
  first bytes match it (`http.DetectContentType`); at most 10 MiB. 200 with `Content-Type` and `Cache-Control:
  private, max-age=3600`, else 404.

### WebSocket `/ws`

Messages are JSON objects with a `type` field, defined in `protocol/schema.json`.

- Phone → relay: `auth {token}` (must be the first message, within 5 s), `subscribe {session_id, after_seq?}`,
  `unsubscribe {session_id}`, `prompt {client_msg_id, session_id, text, attachments?}` (`attachments`: 1-4 unique
  upload ids; `text` may be `""` only with at least one; an unknown id is `bad_request` "unknown attachment", more than
  7 MiB of images in one prompt `too_large`; steers stay text only), `stop {run_id}`, `steer {run_id, text}`,
  `approve {run_id, request_id, choice: once|session|deny}`, `ping`.
- Relay → phone: `ready {device, server_version}`, `event {session_id, run_id, seq, kind, data, ts}` (`ts` in unix
  ms), `run {run_id, session_id, status}` (`running`, `waiting_for_approval`, `completed`, `failed`, `cancelled`; live
  only, and sent after a `subscribe` replay for the active run; a `subscribe` without `after_seq` to a run that is
  `waiting_for_approval` first gets that run's pending `approval.requested` event again, with its original `seq`, so
  a client that connects late can answer it), `ack {client_msg_id, run_id, replayed}`,
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
  over 8 KiB). Plus one of the relay's own, not from Hermes: `user.message {kind: prompt|steer, text, device,
  client_msg_id?, attachments?}`, a user's message as the relay accepted it, so every subscribed device shows it at
  once (#30; messaging servers push the message itself). A prompt's is its run's first event (none for a replayed
  `client_msg_id`); a steer's comes right before `steer.queued`. Hermes' history has it too once stored: a prompt
  ~40 ms after the run starts, a steer when Hermes takes it, so apps dedupe by their outbox and by text and time.
  Apps that predate it drop the frame (both decode strictly and skip what they can't read), so no opt-in. `message.delta` is dropped (streaming is off). `data` keeps only the fields the app needs (previews,
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
  Never message text, approval commands, tokens, session titles, tags or digests.
- 64 KB per WebSocket message (bigger closes the socket, 1009), 20 prompts a minute per device, at most 5 devices and
  3 sockets per device (a 4th closes the oldest), 10 pairing attempts a minute per client IP (30 overall), 4
  requests waiting on Hermes per socket. A socket that falls 8192 messages behind is closed; the app resubscribes.
- WebSocket `Origin` is not checked: the app authenticates with a token in its first message, never a cookie.

## Android app

- A native app in its own repo (decided 2026-11-02), replacing the Expo app (`apps/mobile`, removed the same day; its
  code is at the tag `expo-archive`). Usability follows trismegistus-mac: its sections (Chat, GitHub, News, Agents,
  Jobs, Commands), the chat kind tabs, the command transcript, images in the composer, approvals with Once / Session
  / Deny in the app and in the notification, and the Endfield look from its design tokens.
- Carried over from the Expo backlog as requirements (those issues are closed as superseded): a ping/pong check so a
  dead socket never shows Connected (#6); resubscribe from the last `seq` and resend what is pending after a drop
  (#8); the duplicate filter keeps new events after `replay_gap` (#10); late history never shows an answer twice
  (#11); Stop, steer, reconnect with backoff, settings and an offline cache (#14); approvals and notifications (#15);
  an icon and splash (#17). trismegistus-mac's TrismegistusKit already implements and tests the #6-#11 rules.
- Lives in `xMuramasa/trismegistus-android` (spec v1 there, 2026-11-02): Kotlin + Jetpack Compose, Penpot phone
  frames first, the phone paired as a `manager`, notifications through self-hosted ntfy / UnifiedPush (relay side:
  #28), Chat with approvals and notifications first.

## Dev setups

- **Apps against the homelab relay:** `https://hermes.lab.x-muramasa.dev` over the tailnet.
- **Relay on the Mac:** `cd relay && go run ./cmd/trismegistus-relay` with `HERMES_URL` pointing at an SSH tunnel
  (`ssh -N -L 8642:127.0.0.1:8642 optiplex`) and a local `DATA_DIR`. The app's relay URL is a setting, so it can point at either.

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
  approval prompt is the only gate. A biometric check can be added later.
- Push notifications to Android devices (#28, through the homelab's ntfy, no Firebase) carry no content: one word,
  `approval` or `run`; the app fetches the details over its own socket.

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

Phases 2 and 3 were the Expo app: 2a was built (5e2d2c8) and never checked on the phone, 2b and 3 never started. On
2026-11-02 they were superseded by the native Android app, which gets its own phases in its repo. Since then the
relay has gained the cron, news, model and conversation-management endpoints from phase 4 for the Mac app.

## Answered (2026-10-23)

- Address: `hermes.lab.x-muramasa.dev`.
- Pairing: QR printed by a command on optiplex (one user for now).
- Approvals: a modal in the app, no fingerprint.
- Code: `~/Projects/trismegistus` on optiplex; the Mac gets a clone.
- Name: Trismegistus (app, repo, relay container).
