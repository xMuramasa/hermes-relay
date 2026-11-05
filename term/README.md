# trismegistus-term

The host helper behind the relay's `/api/terminal` (SPEC.md, "Terminal"): the Mac app's terminal drawer is a shell on
optiplex as your user, and the relay runs in a read-only container that can't start host processes. This user service
owns the shells: one connection on its Unix socket is one shell. Go standard library plus `github.com/creack/pty`
(MIT). It refuses to run as root.

## Socket

`~/.local/state/trismegistus-term/term.sock`, directory 0750, socket 0660, owned by you and your group. Those modes are
the security boundary: this socket hands out a shell, so unlike the agents socket it is not world-writable. Only your
user and that group can connect; the relay's container joins the group (`group_add: ["1000"]`). Flag: `-socket`.

## Protocol

The relay sends one JSON line, `{"device":"macbook","cols":80,"rows":24}`, within 5 s. The device is a paired name
(`[A-Za-z0-9._-]{1,64}`), `cols` and `rows` are 1-1000. The helper answers with one JSON line:

- `{"host":"optiplex","shell":"/bin/bash"}`: the shell is running.
- `{"error":"busy"}`: 4 shells are open. `{"error":"bad_open"}` or `{"error":"start_failed"}`: anything else. The
  connection closes after an error.

After that the stream is frames, both ways: one type byte, a big-endian uint16 payload length (at most 32 KiB), the
payload.

| type | direction | payload |
|---|---|---|
| 0 data | both | PTY bytes |
| 1 resize | relay to helper | cols, rows: two big-endian uint16s |
| 2 exit | helper to relay | exit status, big-endian int32 (128 + the signal for a killed shell); the connection closes after it |
| 3 ping | helper to relay | none; ignored by the relay |

A ping exists for one case: when a program stops reading, the helper is stuck writing input to the PTY and can't read
the connection, so it would not see the relay hang up. A stalled write sends a ping every second, and the failed write
tells it the relay is gone.

Neither side buffers beyond one frame, so a slow reader slows the writer.

## Shells

The shell is the passwd entry's, started as a login shell (`/bin/bash -l`) in your home directory on a PTY, in its own
session and process group. The environment is `HOME`, `USER`, `LOGNAME`, `SHELL`, `PATH`, `LANG`,
`TERM=xterm-256color`, `COLORTERM=truecolor` and `TRISMEGISTUS_DEVICE=<name>`, nothing else from the helper's own
(`PATH` and `LANG` take the helper's values, then the login shell's profile applies as usual).

- A closed connection ends the shell: SIGHUP to its session, then SIGKILL to whatever is left after 5 s. The session,
  not only the process group, because an interactive shell puts each job in its own group.
- A shell that exits sends `exit`, closes the connection and ends what it left behind in its session the same way.
- At most 4 shells at once; the fifth gets `busy`. A slot frees when its session is gone.
- A process that starts its own session (`setsid`, tmux, a daemon) outlives its shell, which is how you'd keep
  something running. Only stopping the unit ends those.

Each open and close is logged with the device, pid, duration and exit status. Never the stream.

## Install (optiplex, as your user)

```sh
cd ~/Projects/trismegistus/term
go build -o ~/.local/bin/trismegistus-term .
mkdir -p ~/.local/state/trismegistus-term ~/.config/systemd/user
chmod 750 ~/.local/state/trismegistus-term
cp trismegistus-term.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now trismegistus-term
systemctl --user status trismegistus-term      # "serving" with the socket path
ls -l ~/.local/state/trismegistus-term/        # term.sock, srw-rw----
```

Make the directory before the relay's container starts: Docker would otherwise create the missing mount source as
root, and the helper could no longer make its socket there. User services need lingering, as for
trismegistus-agents (`sudo loginctl enable-linger $USER`).

Then the relay container needs the socket, in compose (Hermes does this):

```yaml
group_add: ["1000"]
volumes:
  - /home/muramasa/.local/state/trismegistus-term:/run/term:ro
environment:
  TERM_SOCKET: /run/term/term.sock
```

After an update: rebuild, then `systemctl --user restart trismegistus-term` (open shells end). Logs:
`journalctl --user -u trismegistus-term`.

**Kill switch:** `systemctl --user stop trismegistus-term` ends every shell at once, and everything else in the unit's
control group within 8 s, daemons started from a shell included. To end one device's shells, use the relay:
`devices terminal NAME off` or `devices revoke NAME`.

## Tests

`go -C term test ./...`: real `/bin/sh` shells on real PTYs, on a socket in a temp dir with a fake account and home.
They cover echo, the environment, a resize `stty size` sees, exit statuses, closing the connection (jobs, a child
ignoring SIGHUP, and input stalled on a program that stopped reading), the limit, stopping the helper and the socket's
modes.
