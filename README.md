# Trismegistus

An Android companion app for [Hermes Agent](https://hermes-agent.nousresearch.com) running on a home server, plus
the small relay server the app talks to. See [SPEC.md](SPEC.md).

```
phone (Expo app) --WebSocket--> relay (Bun) --HTTP/SSE--> Hermes API server (localhost only)
```

Status: phase 0 (probing the Hermes API).

## Layout

- `SPEC.md`: what it does and why.
- `probe/`: phase 0 scripts and the recorded Hermes event streams used as test fixtures.
- `apps/relay`, `apps/mobile`, `packages/protocol`: coming in phases 1 and 2.
