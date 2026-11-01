# trismegistus-agents

A small host helper for the relay's `/api/agents` endpoints (SPEC.md, "Background agents"). The relay runs in a
read-only container without the host's PIDs, so this user service does the host side: it lists Hermes' background
processes and Claude Code delegate runs, turns a run's `stream-json` transcript into readable steps, and stops a
process it lists. Go standard library only.

It reads only `~/.hermes/processes.json`, `/proc/<pid>/stat`, and the `<name>.stream.jsonl` and `<name>-progress.md`
files of delegate runs in the runs directory. Never environment variables, never other files.

HTTP over a Unix socket, `$XDG_RUNTIME_DIR/trismegistus-agents/agents.sock` by default (directory 0755, socket 0666,
so the relay container's user can connect):

- `GET /agents` → `{"agents": [...]}`
- `GET /agents/{id}/transcript?after=N` → `{"steps": [...], "next", "alive"}`
- `POST /agents/{id}/stop` → 202 `{"stopping": true}`

Flags: `-socket`, `-processes` (default `~/.hermes/processes.json`), `-runs` (default
`~/.hermes/cache/scratch/tmac/wt`).

## Install (optiplex, as your user)

```sh
cd ~/Projects/trismegistus/agents
go build -o ~/.local/bin/trismegistus-agents .
mkdir -p ~/.config/systemd/user
cp trismegistus-agents.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now trismegistus-agents
systemctl --user status trismegistus-agents      # "serving" with the socket path
ls -l /run/user/$(id -u)/trismegistus-agents/    # agents.sock, srw-rw-rw-
```

User services run only while you are logged in unless lingering is on (`loginctl show-user $USER -p Linger`; turn
it on with `sudo loginctl enable-linger $USER`; Hermes' own user services need it too).

Then the relay container needs the socket directory mounted and `AGENTS_SOCKET` set (Hermes does this in compose):

```yaml
volumes:
  - /run/user/1000/trismegistus-agents:/run/agents:ro
environment:
  AGENTS_SOCKET: /run/agents/agents.sock
```

A read-only bind mount still lets the relay connect to the socket. After an update: rebuild, then
`systemctl --user restart trismegistus-agents`. Logs: `journalctl --user -u trismegistus-agents`.

Anyone who can reach the socket can stop the listed processes; it lives under `/run/user/<uid>` (mode 0700), so
only you and the relay container (through the mount) can.

## Tests

`go -C agents test ./...`: fake `processes.json`, `/proc` and run files in temp dirs, and real `sleep` children
(started by the test, in their own process group) for stopping. `systemctl` is never called in tests.
