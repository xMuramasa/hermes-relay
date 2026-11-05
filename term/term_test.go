package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const wait = 10 * time.Second

// startHelper serves a helper with /bin/sh on a socket in a temp dir (a short one: socket paths are limited).
func startHelper(t *testing.T) (*Helper, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "term")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state", "term.sock")
	ln, err := listen(path)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHelper(Account{Name: "tester", Home: home, Shell: "/bin/sh"}, "testhost", slog.New(slog.DiscardHandler))
	h.Grace = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return h, path
}

// session is the relay's side of one connection.
type session struct {
	t        *testing.T
	c        net.Conn
	br       *bufio.Reader
	mu       sync.Mutex
	out      strings.Builder
	exit     chan int
	finished chan struct{}
}

// dial sends the open line and returns the helper's one-line answer with the connection.
func dial(t *testing.T, path string, cols, rows int) (net.Conn, *bufio.Reader, map[string]string) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, `{"device":"macbook","cols":%d,"rows":%d}`+"\n", cols, rows)
	_ = c.SetReadDeadline(time.Now().Add(wait))
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(line, &out); err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	_ = c.SetReadDeadline(time.Time{})
	return c, br, out
}

func open(t *testing.T, path string) *session {
	t.Helper()
	c, br, out := dial(t, path, 80, 24)
	if out["host"] != "testhost" || out["shell"] != "/bin/sh" {
		t.Fatalf("open: %v", out)
	}
	s := &session{t: t, c: c, br: br, exit: make(chan int, 1), finished: make(chan struct{})}
	go func() {
		defer close(s.finished)
		buf := make([]byte, maxFrame)
		for {
			typ, p, err := readFrame(br, buf)
			if err != nil {
				return
			}
			switch typ {
			case frameData:
				s.mu.Lock()
				s.out.Write(p)
				s.mu.Unlock()
			case frameExit:
				s.exit <- int(int32(binary.BigEndian.Uint32(p)))
			}
		}
	}()
	return s
}

func (s *session) send(line string) {
	s.t.Helper()
	if err := writeFrame(s.c, frameData, []byte(line+"\n")); err != nil {
		s.t.Fatal(err)
	}
}

// expect waits for a regexp in the output and returns its first group.
func (s *session) expect(re string) string {
	s.t.Helper()
	r := regexp.MustCompile(re)
	for end := time.Now().Add(wait); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		out := s.out.String()
		s.mu.Unlock()
		if m := r.FindStringSubmatch(out); m != nil {
			return m[len(m)-1]
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t.Fatalf("no %q in %q", re, s.out.String())
	return ""
}

// gone waits for a process to end, a zombie counts.
func gone(t *testing.T, pid string) {
	t.Helper()
	for end := time.Now().Add(wait); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		b, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			return
		}
		if state, _, ok := parseStat(b); ok && state == "Z" {
			return
		}
	}
	t.Fatalf("process %s is still alive", pid)
}

func alive(pid string) bool {
	n, _ := strconv.Atoi(pid)
	return syscall.Kill(n, 0) == nil
}

func TestEchoAndEnvironment(t *testing.T) {
	t.Setenv("FAKEFAKE_HELPER_ONLY", "FAKEFAKE")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LANG", "C.UTF-8")
	h, path := startHelper(t)
	s := open(t, path)
	s.send("echo tok$((40+2))en")
	s.expect("tok42en")
	s.send(`echo "[$(pwd)|$HOME|$USER|$LOGNAME|$SHELL|$TERM|$COLORTERM|$TRISMEGISTUS_DEVICE|$PATH|$LANG|${FAKEFAKE_HELPER_ONLY:-unset}]"`)
	home := regexp.QuoteMeta(h.Account.Home)
	s.expect(`\[` + home + `\|` + home + `\|tester\|tester\|/bin/sh\|xterm-256color\|truecolor\|macbook\|/usr/bin:/bin[^|]*\|C\.UTF-8\|unset\]`) // a login shell's /etc/profile may add to PATH
	// Own session and process group.
	s.send(`echo "ids=$$,$(ps -o pgid= -p $$ | tr -d ' '),$(ps -o sid= -p $$ | tr -d ' ')="`)
	if ids := strings.Split(s.expect(`ids=(\d+,\d+,\d+)=`), ","); ids[0] != ids[1] || ids[0] != ids[2] {
		t.Fatalf("pid, pgid, sid: %v", ids)
	}
}

func TestResize(t *testing.T) {
	_, path := startHelper(t)
	s := open(t, path)
	s.send(`echo "size=$(stty size)="`)
	s.expect("size=24 80=")
	var p [4]byte
	binary.BigEndian.PutUint16(p[:], 132)
	binary.BigEndian.PutUint16(p[2:], 50)
	if err := writeFrame(s.c, frameResize, p[:]); err != nil {
		t.Fatal(err)
	}
	s.send(`echo "size=$(stty size)="`)
	s.expect("size=50 132=")
}

func TestExitStatus(t *testing.T) {
	_, path := startHelper(t)
	for shellCmd, want := range map[string]int{"exit 7": 7, "exit 0": 0, "kill -KILL $$": 137} {
		s := open(t, path)
		s.send("echo last words; " + shellCmd)
		select {
		case got := <-s.exit:
			if got != want {
				t.Fatalf("%s: status %d", shellCmd, got)
			}
		case <-time.After(wait):
			t.Fatalf("%s: no exit frame", shellCmd)
		}
		select {
		case <-s.finished: // the connection closed after it
		case <-time.After(wait):
			t.Fatalf("%s: connection stayed open", shellCmd)
		}
		if shellCmd != "kill -KILL $$" {
			s.expect("last words\\s") // the output came before the exit, typed line aside
		}
	}
}

func TestCloseKillsShellAndChildren(t *testing.T) {
	_, path := startHelper(t)
	s := open(t, path)
	// A job (its own process group in an interactive shell), one ignoring SIGHUP, and a child in the foreground.
	s.send(`sleep 300 & echo "job=$!="`)
	job := s.expect(`job=(\d+)=`)
	s.send(`(trap '' HUP; exec sleep 301) & echo "stubborn=$!="`)
	stubborn := s.expect(`stubborn=(\d+)=`)
	s.send(`echo "shell=$$="`)
	shell := s.expect(`shell=(\d+)=`)
	s.send("sleep 302")
	if !alive(job) || !alive(stubborn) || !alive(shell) {
		t.Fatal("not started")
	}
	time.Sleep(100 * time.Millisecond)
	s.c.Close()
	gone(t, job)
	gone(t, shell)
	gone(t, stubborn) // SIGKILL, after the grace time
}

// A program that stopped reading must not hide the relay hanging up: the helper is stuck writing to the PTY, not reading.
func TestCloseWhileInputStalled(t *testing.T) {
	h, path := startHelper(t)
	s := open(t, path)
	s.send(`echo "shell=$$="`)
	shell := s.expect(`shell=(\d+)=`)
	s.send(`stty raw -echo; echo ready=; sleep 300`)
	s.expect("ready=")
	var p [4]byte // a resize first: its ioctl must not turn the PTY blocking
	binary.BigEndian.PutUint16(p[:], 100)
	binary.BigEndian.PutUint16(p[2:], 40)
	if err := writeFrame(s.c, frameResize, p[:]); err != nil {
		t.Fatal(err)
	}
	flood := make(chan struct{})
	go func() {
		defer close(flood)
		chunk := make([]byte, maxFrame)
		for i := range chunk {
			chunk[i] = 'x'
		}
		_ = s.c.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		for writeFrame(s.c, frameData, chunk) == nil {
		}
	}()
	<-flood // the socket is full: the PTY stopped taking input
	s.c.Close()
	gone(t, shell)
	for end := time.Now().Add(wait); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if len(h.slots) == 0 {
			return
		}
	}
	t.Fatal("the slot was not released")
}

func TestLimit(t *testing.T) {
	_, path := startHelper(t)
	var first *session
	for i := range maxShells {
		s := open(t, path)
		if i == 0 {
			first = s
		}
	}
	c, br, out := dial(t, path, 80, 24)
	if out["error"] != "busy" {
		t.Fatalf("fifth: %v", out)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("the refused connection stayed open: %v", err)
	}
	c.Close()
	first.send("exit")
	for end := time.Now().Add(wait); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if _, _, out := dial(t, path, 80, 24); out["error"] == "" {
			return
		}
	}
	t.Fatal("no slot came free")
}

func TestStopEndsEveryShell(t *testing.T) {
	dir, err := os.MkdirTemp("", "term")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "term.sock")
	ln, err := listen(path)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHelper(Account{Name: "tester", Home: dir, Shell: "/bin/sh"}, "testhost", slog.New(slog.DiscardHandler))
	h.Grace = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx, ln) }()
	s := open(t, path)
	s.send(`echo "shell=$$="`)
	shell := s.expect(`shell=(\d+)=`)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(wait):
		t.Fatal("Serve did not return")
	}
	gone(t, shell)
}

func TestSocketModes(t *testing.T) {
	_, path := startHelper(t)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode %v", fi.Mode())
	}
	if di, err := os.Stat(filepath.Dir(path)); err != nil || di.Mode().Perm() != 0o750 {
		t.Fatalf("directory: %v %v", di, err)
	}
	// A wider directory left from before is narrowed, and a stale socket replaced.
	if err := os.Chmod(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "term")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	stale := filepath.Join(dir, "term.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	l2, err := listen(stale)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o750 {
		t.Fatalf("directory not narrowed: %v", di.Mode())
	}
	if _, err := listen("relative.sock"); err == nil {
		t.Fatal("a relative path must be refused")
	}
}

func TestBadOpenLine(t *testing.T) {
	_, path := startHelper(t)
	for _, line := range []string{
		"not json",
		`{"device":"a b","cols":80,"rows":24}`,
		`{"device":"","cols":80,"rows":24}`,
		`{"device":"mac","cols":0,"rows":24}`,
		`{"device":"mac","cols":80,"rows":100000}`,
	} {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(c, line)
		_ = c.SetReadDeadline(time.Now().Add(wait))
		b, err := io.ReadAll(c)
		c.Close()
		if err != nil || !strings.Contains(string(b), "bad_open") {
			t.Fatalf("%q: %q %v", line, b, err)
		}
	}
}

func TestFrames(t *testing.T) {
	r, w := net.Pipe()
	defer r.Close()
	go func() {
		defer w.Close()
		_ = writeFrame(w, frameData, []byte("hi"))
		_ = writeFrame(w, framePing, nil)
		_, _ = w.Write([]byte{frameData, 0xff, 0xff}) // over maxFrame
	}()
	buf := make([]byte, maxFrame)
	if typ, p, err := readFrame(r, buf); err != nil || typ != frameData || string(p) != "hi" {
		t.Fatalf("%d %q %v", typ, p, err)
	}
	if typ, p, err := readFrame(r, buf); err != nil || typ != framePing || len(p) != 0 {
		t.Fatalf("%d %q %v", typ, p, err)
	}
	if _, _, err := readFrame(r, buf); err == nil {
		t.Fatal("a frame over the limit must fail")
	}
}

func TestLookupAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passwd")
	data := "root:x:0:0:root:/root:/bin/bash\nmuramasa:x:1000:1000:Martin,,,:/home/muramasa:/bin/bash\n\nbroken\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := lookupAccount(path, 1000)
	if err != nil || a != (Account{"muramasa", "/home/muramasa", "/bin/bash"}) {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := lookupAccount(path, 1001); err == nil {
		t.Fatal("unknown uid")
	}
}

func TestParseStat(t *testing.T) {
	if st, sid, ok := parseStat([]byte("42 (a b) c)) S 1 42 43 0 -1")); !ok || st != "S" || sid != 43 {
		t.Fatalf("%q %d %v", st, sid, ok)
	}
	if _, _, ok := parseStat([]byte("garbage")); ok {
		t.Fatal("garbage parsed")
	}
}
