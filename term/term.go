package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
)

// The stream after the open line, both ways (README.md): one type byte, a big-endian uint16 payload length, the payload.
const (
	frameData   byte = 0 // PTY bytes
	frameResize byte = 1 // relay to helper: cols, rows as big-endian uint16s
	frameExit   byte = 2 // helper to relay: the exit status as a big-endian int32, then the connection closes
	framePing   byte = 3 // helper to relay, ignored: see feed

	maxFrame = 32 << 10
	maxSize  = 1000 // cols, rows

	maxShells   = 4
	openTimeout = 5 * time.Second
	drainTime   = 200 * time.Millisecond // for the last output of a shell that exited
	probeEvery  = time.Second
)

var deviceRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`) // pairing's device names

// Account is the passwd entry the shells run as.
type Account struct {
	Name, Home, Shell string
}

// lookupAccount reads the entry of uid from a passwd file. os/user has no shell field.
func lookupAccount(path string, uid int) (Account, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Account{}, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) == 7 && f[2] == strconv.Itoa(uid) {
			return Account{Name: f[0], Home: f[5], Shell: f[6]}, nil
		}
	}
	return Account{}, fmt.Errorf("uid %d is not in %s", uid, path)
}

// Helper starts one shell per connection.
type Helper struct {
	Account Account
	Host    string
	Grace   time.Duration // SIGHUP to SIGKILL
	Proc    string        // /proc
	Log     *slog.Logger

	slots chan struct{}
}

// NewHelper makes a helper with the limits of README.md.
func NewHelper(acct Account, host string, log *slog.Logger) *Helper {
	return &Helper{Account: acct, Host: host, Grace: 5 * time.Second, Proc: "/proc", Log: log, slots: make(chan struct{}, maxShells)}
}

// Serve accepts connections until ctx is done, then ends every shell and returns.
func (h *Helper) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Go(func() { h.handle(ctx, conn) })
	}
}

func writeFrame(w io.Writer, typ byte, p []byte) error {
	b := make([]byte, 3+len(p))
	b[0] = typ
	binary.BigEndian.PutUint16(b[1:], uint16(len(p)))
	copy(b[3:], p)
	_, err := w.Write(b)
	return err
}

// readFrame reads one frame into buf (maxFrame long).
func readFrame(r io.Reader, buf []byte) (byte, []byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[1:]))
	if n > len(buf) {
		return 0, nil, errors.New("frame too long")
	}
	_, err := io.ReadFull(r, buf[:n])
	return hdr[0], buf[:n], err
}

func reply(conn net.Conn, v any) {
	b, _ := json.Marshal(v)
	_, _ = conn.Write(append(b, '\n'))
}

type failure struct {
	Error string `json:"error"`
}

// handle is one connection: the open line, then one shell until either side ends.
func (h *Helper) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	_ = conn.SetReadDeadline(time.Now().Add(openTimeout))
	br := bufio.NewReaderSize(conn, 4096)
	line, err := br.ReadSlice('\n')
	var open struct {
		Device     string
		Cols, Rows int
	}
	if err != nil || json.Unmarshal(line, &open) != nil || !deviceRe.MatchString(open.Device) ||
		open.Cols < 1 || open.Cols > maxSize || open.Rows < 1 || open.Rows > maxSize {
		reply(conn, failure{"bad_open"})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		h.Log.Warn("shell refused: busy", "device", open.Device)
		reply(conn, failure{"busy"})
		return
	}

	a := h.Account
	cmd := exec.Command(a.Shell, "-l")
	cmd.Dir = a.Home
	cmd.Env = []string{"HOME=" + a.Home, "USER=" + a.Name, "LOGNAME=" + a.Name, "SHELL=" + a.Shell,
		"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin"), "LANG=" + envOr("LANG", "C.UTF-8"),
		"TERM=xterm-256color", "COLORTERM=truecolor", "TRISMEGISTUS_DEVICE=" + open.Device}
	ctl, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(open.Cols), Rows: uint16(open.Rows)}) // Setsid: own session and group
	if err != nil {
		h.Log.Error("shell failed to start", "device", open.Device, "err", err.Error())
		reply(conn, failure{"start_failed"})
		return
	}
	ptmx, err := pollable(ctl)
	if err != nil {
		h.Log.Error("shell failed to start", "device", open.Device, "err", err.Error())
		reply(conn, failure{"start_failed"})
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return
	}
	defer ptmx.Close()
	start, pid := time.Now(), cmd.Process.Pid
	h.Log.Info("shell opened", "device", open.Device, "pid", pid)
	reply(conn, map[string]string{"host": h.Host, "shell": a.Shell})

	var writes sync.Mutex
	send := func(typ byte, p []byte) error {
		writes.Lock()
		defer writes.Unlock()
		return writeFrame(conn, typ, p)
	}
	hangup, hungUp := context.WithCancel(context.Background()) // the connection is gone
	defer hungUp()
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	output := make(chan struct{})
	go func() {
		defer close(output)
		buf := make([]byte, maxFrame)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 && send(frameData, buf[:n]) != nil {
				hungUp()
				return
			}
			if err != nil { // EIO: the last tty closed. The shell's end comes from Wait, a job may hold the tty after it.
				return
			}
		}
	}()
	input := make(chan struct{})
	go func() {
		defer close(input)
		defer hungUp()
		buf := make([]byte, maxFrame)
		for {
			typ, p, err := readFrame(br, buf)
			if err != nil {
				return
			}
			switch {
			case typ == frameData:
				if !feed(ptmx, p, send) {
					return
				}
			case typ == frameResize && len(p) == 4:
				cols, rows := binary.BigEndian.Uint16(p), binary.BigEndian.Uint16(p[2:])
				if cols >= 1 && cols <= maxSize && rows >= 1 && rows <= maxSize {
					_ = setSize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
				}
			}
		}
	}()

	select {
	case <-hangup.Done():
		h.endSession(pid)
		<-waited
	case <-waited:
		_ = ptmx.SetReadDeadline(time.Now().Add(drainTime))
		_ = conn.SetWriteDeadline(time.Now().Add(h.Grace))
		<-output
		var status [4]byte
		binary.BigEndian.PutUint32(status[:], uint32(exitStatus(cmd.ProcessState)))
		_ = send(frameExit, status[:])
		conn.Close()
		h.endSession(pid)
	}
	ptmx.Close()
	conn.Close()
	<-output
	<-input
	h.Log.Info("shell closed", "device", open.Device, "pid", pid, "duration_ms", time.Since(start).Milliseconds(),
		"exit_status", exitStatus(cmd.ProcessState))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// pollable replaces the PTY with a dup whose reads and writes take deadlines and end on Close. Any File.Fd call turns
// the shared file description blocking again, and pty's ioctls make one: use setSize, not pty.Setsize, on the result.
func pollable(ptmx *os.File) (*os.File, error) {
	defer ptmx.Close()
	fd, _, errno := syscall.Syscall(syscall.SYS_FCNTL, ptmx.Fd(), syscall.F_DUPFD_CLOEXEC, 0)
	if errno != 0 {
		return nil, errno
	}
	if err := syscall.SetNonblock(int(fd), true); err != nil {
		syscall.Close(int(fd))
		return nil, err
	}
	return os.NewFile(fd, "ptmx"), nil
}

func setSize(ptmx *os.File, ws *pty.Winsize) error {
	c, err := ptmx.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := c.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(ws)))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// feed writes input to the PTY and reports whether the connection is still up. A program that stopped reading stalls
// the write, and so the read that would see the relay hang up: a stalled write probes the connection with a ping
// instead. Input for a shell that is gone is dropped, the exit comes from Wait.
func feed(ptmx *os.File, p []byte, send func(byte, []byte) error) bool {
	for len(p) > 0 {
		_ = ptmx.SetWriteDeadline(time.Now().Add(probeEvery))
		n, err := ptmx.Write(p)
		p = p[n:]
		if errors.Is(err, os.ErrDeadlineExceeded) {
			if send(framePing, nil) != nil {
				return false
			}
		} else if err != nil {
			return true
		}
	}
	return true
}

// exitStatus is the shell's convention: the status, or 128 + the signal.
func exitStatus(s *os.ProcessState) int {
	if ws, ok := s.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return s.ExitCode()
}

// endSession sends SIGHUP to the shell's session, then SIGKILL to what is left after the grace time. The session and
// not only the shell's process group: an interactive shell gives each job its own group.
// shortcut: a process that calls setsid itself (tmux, a daemon) escapes this; `systemctl --user stop` still ends it
// through the unit's control group. Upgrade to a cgroup per shell if a revoke has to reach those too.
func (h *Helper) endSession(sid int) {
	if h.signalSession(sid, syscall.SIGHUP) == 0 {
		return
	}
	for end := time.Now().Add(h.Grace); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if h.signalSession(sid, 0) == 0 {
			return
		}
	}
	for range 100 { // a forking shell can add to the session between two scans
		if h.signalSession(sid, syscall.SIGKILL) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.Log.Error("session survived SIGKILL", "pid", sid)
}

// signalSession signals the live processes of a session and returns how many there were.
func (h *Helper) signalSession(sid int, sig syscall.Signal) int {
	ents, err := os.ReadDir(h.Proc)
	if err != nil {
		h.Log.Error("reading the process list failed", "err", err.Error())
		return 0
	}
	n := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(h.Proc, e.Name(), "stat"))
		if err != nil {
			continue // it exited
		}
		if state, session, ok := parseStat(b); ok && session == sid && state != "Z" {
			n++
			_ = syscall.Kill(pid, sig)
		}
	}
	return n
}

// parseStat reads the state and session from /proc/<pid>/stat: pid (comm) state ppid pgrp session ... comm may hold
// spaces and parentheses, so fields count from the last ')'.
func parseStat(b []byte) (state string, session int, ok bool) {
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	if len(f) < 4 {
		return "", 0, false
	}
	session, err := strconv.Atoi(f[3])
	return f[0], session, err == nil
}
