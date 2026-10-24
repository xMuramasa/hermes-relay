# Trismegistus

An Android companion app for [Hermes Agent](https://hermes-agent.nousresearch.com) running on a home server, plus
the small relay server the app talks to. See [SPEC.md](SPEC.md).

```
phone (Expo app) --WebSocket--> relay (Go) --HTTP/SSE--> Hermes API server (localhost only)
```

Status: phase 0 done (`probe/FINDINGS.md`); phase 1 (relay) next.

## Layout

- `SPEC.md`: what it does and why.
- `probe/`: phase 0 scripts and the recorded Hermes event streams used as test fixtures.
- `protocol/`: the WebSocket protocol (JSON Schema + example messages), shared by relay and app.
- `relay/`: the Go relay (phase 1).
- `apps/mobile`: the Expo app (phase 2).
