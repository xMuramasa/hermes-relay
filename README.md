# Trismegistus

The relay server and WebSocket protocol behind the companion apps for
[Hermes Agent](https://hermes-agent.nousresearch.com) running on a home server: the native macOS app
([trismegistus-mac](https://github.com/xMuramasa/trismegistus-mac)) and a native Android app (planned, its own repo).
See [SPEC.md](SPEC.md).

```
apps (macOS, Android) --WebSocket--> relay (Go) --HTTP/SSE--> Hermes API server (localhost only)
```

Status: phase 0 done (`probe/FINDINGS.md`); phase 1 (protocol + relay) built, tested and deployed, since extended
for the Mac app. The Expo Android app (`apps/mobile`, phase 2a) was removed on 2026-11-02 for a native build; its
code is at the tag `expo-archive`.

## Layout

- `SPEC.md`: what it does and why.
- `probe/`: phase 0 scripts and the recorded Hermes event streams used as test fixtures.
- `protocol/`: the WebSocket protocol, shared by the relay and the apps: `schema.json` (JSON Schema 2020-12),
  `examples/valid/` and `examples/invalid/`, and a short guide in `protocol/README.md`.
- `relay/`: the Go relay (one module, `github.com/xMuramasa/trismegistus/relay`).
  - `cmd/trismegistus-relay`: the binary and its subcommands.
  - `internal/config`: settings from the environment.
  - `internal/protocol`: message types and validation against an embedded copy of `protocol/schema.json`.
  - `internal/hermes`: Hermes REST + SSE client (timeouts, dead-stream detection, error mapping).
  - `internal/hub`: runs, Hermes event mapping, event numbering and storage, fan-out, re-attach after drops and
    restarts.
  - `internal/server`: HTTP routes (REST allowlist, pairing, health), WebSocket, auth, rate limits.
  - `internal/store`: SQLite (devices, pairing codes, runs, event log), schema migrations.
  - `internal/pairing`: tokens, pairing codes, the QR code.
  - `internal/fakehermes`: the fake Hermes the tests run against (replays `probe/fixtures/`).
  - `Dockerfile`: build context `relay/`.
- `agents/`: the host helper behind `/api/agents` (its own module and README).
- `term/`: the host helper behind `/api/terminal`, which owns the shells of the Mac app's terminal drawer (its own
  module and README).

## Protocol

Edit `protocol/schema.json`, add examples, then copy it into the relay:

```sh
cp protocol/schema.json relay/internal/protocol/schema.json
```

A relay test fails while the two differ.

## Relay

### Tests

```sh
cd relay
go vet ./... && go test -count=1 ./...
gofmt -l .        # must print nothing
# race detector (needs cgo, so in Docker), from the repo root:
docker run --rm -v "$PWD":/src -w /src/relay golang:1.27.3-bookworm go test -race -count=1 ./...
```

The tests need no network and no real Hermes: `internal/fakehermes` replays the phase 0 recordings with
controllable timing (holds, cut streams, silent streams, relay restarts). Every message the relay sends in the tests
is checked against the schema, and the suite fails if any log line contains a token, pairing code, prompt or steer
text, approval command or the Hermes key.

### Run locally

```sh
cd relay
HERMES_API_KEY=... DATA_DIR=./data PUBLIC_URL=http://localhost:8650 go run ./cmd/trismegistus-relay
```

### Docker

```sh
docker build -t trismegistus-relay --build-arg VERSION=$(git describe --always --dirty) relay
```

Distroless static image (~25 MB, runs as `nonroot`), binary at `/usr/local/bin/trismegistus-relay`, data in
`/data` (mount a named volume), `HEALTHCHECK` via the `healthcheck` subcommand. The compose file and Caddy config
live in the homelab repo.

### Environment

| Variable | Default | |
|---|---|---|
| `HERMES_API_KEY` | (required for `serve`) | Hermes API server key; never logged |
| `HERMES_URL` | `http://127.0.0.1:8642` | Hermes API server |
| `RELAY_ADDR` | `127.0.0.1:8650` | listen address (`172.18.0.1:8650` in the homelab) |
| `PUBLIC_URL` | (required for `pair`) | the relay's URL as the app sees it; goes in the pairing QR |
| `DATA_DIR` | `/data` | holds `relay.db` (SQLite, WAL) |
| `TRUSTED_PROXIES` | `127.0.0.1/32,172.16.0.0/12` | peers whose `X-Forwarded-For` is honored (rate limits) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `TERM_SOCKET` | (none) | the host helper `trismegistus-term`'s socket, an absolute path; empty and `/api/terminal` answers `not_configured` |

### Commands

```sh
trismegistus-relay [serve]                       # run the relay (default)
trismegistus-relay pair --name pixel-8 [--replace]  # QR + code for a new device (10 min, single use)
trismegistus-relay devices list
trismegistus-relay devices terminal macbook on   # or off: its terminals close within ~2 s
trismegistus-relay devices revoke pixel-8        # its sockets and terminals close within ~2 s
trismegistus-relay healthcheck                   # exit 0 if GET /healthz on RELAY_ADDR is ok
trismegistus-relay version
```

In the container: `docker exec -it trismegistus-relay trismegistus-relay pair --name pixel-8`. `pair` and
`devices` open the same database as the running server.
