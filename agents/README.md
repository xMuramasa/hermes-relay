# trismegistus-agents

A small host helper for the relay's `/api/agents` endpoints (SPEC.md, "Background agents"). The relay runs in a
read-only container without the host's PIDs, so this user service does the host side: it lists Hermes' background
processes and Claude Code delegate runs, turns a run's `stream-json` transcript into readable steps, and stops a
process it lists. Go standard library only.

It reads only `~/.hermes/processes.json`, `/proc/<pid>/stat`, the `<name>.stream.jsonl` and `<name>-progress.md`
files of delegate runs in the runs directory, and the images `/media` is asked for (below). Never environment
variables, never other files.

HTTP over a Unix socket, `$XDG_RUNTIME_DIR/trismegistus-agents/agents.sock` by default (directory 0755, socket 0666,
so the relay container's user can connect):

- `GET /agents` → `{"agents": [...]}`
- `GET /agents/{id}/transcript?after=N` → `{"steps": [...], "next", "alive"}`
- `POST /agents/{id}/stop` → 202 `{"stopping": true}`
- `GET /media?path=<absolute path>` → the image (for the chat's `MEDIA:` paths and Telegram images; the relay asks only
  for paths that occur in the session's messages). Served only when the path is clean and absolute and, both as asked
  and with symlinks resolved, under your home directory with no component starting with `.` except under
  `~/.hermes/cache/`; a regular file with extension png, jpg, jpeg, gif or webp whose first bytes match it; at most
  10 MiB. 200 with `Content-Type` and `Cache-Control: private, max-age=3600`; anything else is 404 `not_found`.

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
ls -l ~/.local/state/trismegistus-agents/        # agents.sock, srw-rw-rw-
```

User services run only while you are logged in unless lingering is on (`loginctl show-user $USER -p Linger`; turn
it on with `sudo loginctl enable-linger $USER`; Hermes' own user services need it too).

Then the relay container needs the socket directory mounted and `AGENTS_SOCKET` set (Hermes does this in compose):

```yaml
volumes:
  - /home/muramasa/.local/state/trismegistus-agents:/run/agents:ro
environment:
  AGENTS_SOCKET: /run/agents/agents.sock
```

The unit puts the socket in `~/.local/state/trismegistus-agents/` rather than the `$XDG_RUNTIME_DIR` default:
`/run/user/<uid>` is emptied at boot, and if the relay container started before the helper, Docker would create the
missing mount source as root and the helper could no longer make its socket there. The state directory persists
(create it once with `mkdir -p`); the helper replaces a stale socket when it starts.

A read-only bind mount still lets the relay connect to the socket. After an update: rebuild, then
`systemctl --user restart trismegistus-agents`. Logs: `journalctl --user -u trismegistus-agents`.

Anyone who can reach the socket can stop the listed processes; it lives under your home directory (mode 0750), so only you and the relay container (through the mount) can.

## Tests

`go -C agents test ./...`: fake `processes.json`, `/proc` and run files in temp dirs, and real `sleep` children
(started by the test, in their own process group) for stopping. `systemctl` is never called in tests.
