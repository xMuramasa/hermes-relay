# Trismegistus WebSocket protocol (v1)

`schema.json` (JSON Schema 2020-12) defines every message between the app and the relay. `examples/valid/` holds
messages that must pass and `examples/invalid/` messages that must fail; the relay's Go tests and the app's tests both
check every file. Change the schema here, then copy it to `relay/internal/protocol/schema.json` (a Go test fails until
the two are byte-identical).

## Connection

1. Open `wss://<relay>/ws`. Each frame is one JSON text message with a `type`. Max 64 KB per message from the phone;
   bigger ones close the socket (1009).
2. Send `{"type":"auth","token":"trs_..."}` within 5 s. The relay answers `ready {device, server_version}`, or
   `error unauthorized` and closes (1008).
3. `subscribe {session_id, after_seq?}` for each open chat. Events with `seq > after_seq` are replayed first, then
   live ones follow. If the active run is still going, a `run` message with its status follows the replay. If the
   relay no longer has the events after `after_seq` (24 h buffer), it answers `error replay_gap` with
   `ref = session_id`: reload the history over REST and subscribe again without `after_seq`.
4. The relay pings (WebSocket ping) every 25 s and drops the socket after two missed pongs. The app may also send
   `ping` and get `pong`. Runs never depend on a socket: reconnect, resubscribe from the last `seq` seen.

A device has at most 3 sockets; a 4th closes the oldest. A revoked device is disconnected.

## Phone → relay

| type | fields | notes |
|---|---|---|
| `auth` | `token` | first message only |
| `subscribe` | `session_id`, `after_seq?` | |
| `unsubscribe` | `session_id` | |
| `prompt` | `client_msg_id`, `session_id`, `text` | `client_msg_id` = a UUID per message; resending it never starts a second run |
| `stop` | `run_id` | |
| `steer` | `run_id`, `text` | only while the run is `running` |
| `approve` | `run_id`, `request_id`, `choice` | `once` \| `session` \| `deny` (`always` is refused) |
| `ping` | | |

`stop`, `steer` and `approve` need a subscription to the run's session.

## Relay → phone

| type | fields | notes |
|---|---|---|
| `ready` | `device`, `server_version` | after auth |
| `ack` | `client_msg_id`, `run_id`, `replayed` | prompt accepted; `replayed: true` when the id was seen before |
| `event` | `session_id`, `run_id`, `seq`, `kind`, `data`, `ts` | stored and numbered; `seq` counts per session from 1, `ts` in unix ms |
| `run` | `run_id`, `session_id`, `status` | live status: `running`, `waiting_for_approval`, `completed`, `failed`, `cancelled` |
| `error` | `code`, `message`, `ref?` | `ref` = the prompt's `client_msg_id`, the `run_id`, or the `session_id` |
| `pong` | | |

### Event kinds and `data`

| kind | data |
|---|---|
| `tool.started` | `tool`, `preview?` (arguments, cut) |
| `tool.completed` | `tool`, `error`, `duration?` (s), `preview?` (result, cut) |
| `subagent.started` | `subagent_id`, `goal?`, `model?`, `child_session_id?`, `task_index?`, `task_count?` |
| `subagent.completed` | `subagent_id`, `status?`, `summary?`, `duration?` (s) |
| `approval.requested` | `request_id`, `command`, `description?`, `choices` (subset of once/session/deny) |
| `approval.resolved` | `request_id`, `choice` (Hermes' value, may be `always` if resolved elsewhere) |
| `answer` | `text`, `truncated?` (over 256 KiB; the full text is in the history) |
| `run.completed` | `{}` (always right after `answer`) |
| `run.failed` | `error?` |
| `run.cancelled` | `reason?` |
| `steer.queued` | `{}` (a steer reached Hermes) |
| `reconnected` | `reason`: `stream_lost` or `relay_restart`; events may be missing before it, the answer still comes |
| `unknown` | `event` (Hermes' name), `data` (raw, `{}` + `truncated` when over 8 KiB): ignore it |

A turn ends with `answer` + `run.completed`, `run.failed` or `run.cancelled`.

### Error codes

`unauthorized`, `bad_request` (schema or state), `too_large`, `rate_limited` (20 prompts/min per device),
`busy` (a run is active: offer to steer instead), `not_found`, `read_only` (session owned by Telegram/CLI/...),
`replay_gap`, `hermes_unavailable` (try again later), `hermes_error`, `internal`.

## REST (Bearer device token)

`GET /api/sessions`, `POST /api/sessions` (no body; creates an untitled session), `GET /api/sessions/{id}`,
`GET /api/sessions/{id}/messages`, `PATCH /api/sessions/{id} {title}` and `DELETE /api/sessions/{id}` (only
`api_server` sessions). Responses are Hermes' JSON. Errors are `{"error": {"code", "message"}}` with the codes above.

`POST /api/pair {code, name?}` (no auth) → `{token, device: {name, created_at}}`. The device name comes from
`pair --name`; a `name` in the body is ignored. `GET /healthz` → `ok`.
