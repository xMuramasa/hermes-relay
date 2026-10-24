# Phase 0 findings (2026-10-23, Hermes 0.22.2)

Recorded with `probe/probe.py` against `127.0.0.1:8642` on optiplex, each scenario in a throwaway API session on
Claude Haiku 5.5 (deleted afterwards). Recordings are in `fixtures/` (one JSON line per request, response or SSE
frame, with timing `t` in seconds).

## Works as documented

- `POST /api/sessions {title}` → 201 `{session: {id: "api_<unix>_<hex>", ...}}`. Titles must be unique (400
  `invalid_title` otherwise), so the relay adds a suffix or leaves the title empty.
- `POST /v1/runs {input, session_id, model?, provider?}` → 202 `{run_id, status: "started", replayed: false}`.
  The same `Idempotency-Key` again → 202 with the same `run_id`, `replayed: true` and `Idempotency-Replayed: true`.
- `GET /v1/runs/{id}/events` (SSE): no `event:` line; each frame is `data: {event, run_id, timestamp, seq, ...}`.
  `seq` counts from 0 per run. A `: open` comment first, then `: keepalive` every 10 s.
  - `tool.started {tool, preview}` (arguments cut to ~40 characters)
  - `tool.completed {tool, duration, error, preview}` (result, up to 500 characters)
  - `subagent.start {subagent_id, child_session_id, delegation_id, goal, model, ...}`
  - `subagent.complete {status, summary, duration_seconds, input_tokens, output_tokens, cost_usd, ...}`
  - `reasoning.available {text}`: on this provider it carries the final answer text, just before
  - `run.completed {output, usage, runtime: {provider, model}, completed, partial, interrupted}`
  - `run.cancelled {interrupted: true, turn_exit_reason}` after `POST /v1/runs/{id}/stop` (→ 200
    `{status: "stopping"}`); cancelled within ~50 ms during a model call.
- `GET /v1/runs/{id}`: `status` running/completed/cancelled, `last_event`, `output`, `usage`.
- `GET /api/sessions/{id}/messages` → `{data: [{id, role, content, tool_calls, tool_name, timestamp, ...}],
  pagination}`.
- Delegation: `delegate_task` runs in the background. The run completes first ("Subagent dispatched…") and the
  subagent's result arrives as `subagent.complete` on the same stream before `run.completed`; for slower children it
  lands in the session history for the next turn.

## Different from the spec

1. **No streamed text.** Neither `/v1/runs` nor `/api/sessions/{id}/chat/stream` sent text in pieces: the whole
   answer came at the end (`reasoning.available` / `run.completed`, or one `tool.progress` frame named `_thinking`
   on the chat stream). Hermes' `streaming.enabled` is `false` in config, which may be why; it's also possible that
   the Claude subscription provider doesn't stream through the API server. Telegram replies don't stream either.
   Testing needs `streaming.enabled: true` and a gateway restart, and it would also turn on live-edited replies in
   Telegram. Until then the app shows tool progress live and the answer when it's done.
2. **No approval prompt.** `approvals.mode` is `smart`: the `rm -rf` test was judged low-risk and auto-approved
   (`"approval": "Command was flagged (recursive delete) and auto-approved by smart approval."` in the tool result).
   No `approval.request` event was recorded, so the approval modal has no real fixture yet. Getting one needs either
   a command smart approval escalates, or `approvals.mode: manual` (which also makes Telegram ask more often).
3. **Telegram and other sessions are readable.** `GET /api/sessions` lists all 24 sessions (Telegram, CLI, TUI, cron,
   one-shot), and `GET /api/sessions/{id}/messages` returns a Telegram chat's full history (450 rows: user,
   assistant and tool messages). Running new turns in them is still refused (documented 409). So the app can show
   them read-only.

## Not tested yet

- `approval.request` payload and `POST /v1/runs/{id}/approval` (see 2).
- `message.delta` and `message.interim` (see 1).
- `/steer`, `/clarify`, inline images.
- Relay restart during a run (phase 1 tests against a fake Hermes).
