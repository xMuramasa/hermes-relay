package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeStat is a /proc/<pid>/stat line with the given state and start time (field 22).
func fakeStat(pid int, state string, start int64) string {
	return fmt.Sprintf("%d (ba sh) (x)) %s%s %d%s\n", pid, state, strings.Repeat(" 0", 18), start, strings.Repeat(" 0", 10))
}

type env struct {
	t      *testing.T
	h      *Helper
	srv    *httptest.Server
	mu     sync.Mutex
	unit   []string
	active map[string]bool // dispatch units systemctl would list as active
}

func newEnv(t *testing.T, proc string) *env {
	dir := t.TempDir()
	runs := filepath.Join(dir, "wt")
	if proc == "" {
		proc = filepath.Join(dir, "proc")
	}
	for _, d := range []string{runs, proc} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{t: t}
	e.h = &Helper{Processes: filepath.Join(dir, "processes.json"), Runs: []string{runs, filepath.Join(dir, "scratch")}, Proc: proc, Home: dir,
		Grace: 300 * time.Millisecond, Now: time.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ActiveUnits: func() map[string]bool { e.mu.Lock(); defer e.mu.Unlock(); return maps.Clone(e.active) },
		StopUnit:    func(u string) error { e.mu.Lock(); e.unit = append(e.unit, u); e.mu.Unlock(); return nil }}
	e.srv = httptest.NewServer(e.h.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) write(path, s string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) processes(v any) {
	b, _ := json.Marshal(v)
	e.write(e.h.Processes, string(b))
}

func (e *env) do(method, path string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *env) list() map[string]Agent {
	e.t.Helper()
	st, b := e.do("GET", "/agents")
	var out struct{ Agents []Agent }
	if err := json.Unmarshal([]byte(b), &out); st != 200 || err != nil {
		e.t.Fatalf("list: %d %s", st, b)
	}
	m := map[string]Agent{}
	for _, a := range out.Agents {
		m[a.ID] = a
	}
	return m
}

const stream = `{"type":"system","subtype":"hook_started"}
{"type":"system","subtype":"init","cwd":"/home/u/Projects/repo-wt/issue-9","session_id":"x"}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Reading the spec."},{"type":"tool_use","name":"Bash","input":{"command":"go -C relay test\n  ./...","description":"run tests"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"` + "%s" + `"}],"is_error":true}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{"todos":[{"content":"a"}]}}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":7,"duration_ms":125000,"result":"All done."}
`

func TestListAndTranscript(t *testing.T) {
	e := newEnv(t, "")
	now := time.Now()
	// A live delegate run (string numbers, as Hermes has also written them), a plain process, a stale entry whose pid
	// now names another process, and a junk id.
	e.write(filepath.Join(e.h.Proc, "4001", "stat"), fakeStat(4001, "S", 777))
	e.write(filepath.Join(e.h.Proc, "4002", "stat"), fakeStat(4002, "S", 888))
	e.write(filepath.Join(e.h.Proc, "4003", "stat"), fakeStat(4003, "S", 1))
	e.processes([]map[string]any{
		{"session_id": "proc_aaa", "command": "bash ~/wt/live-1-launch.sh", "pid": "4001", "host_start_time": "777",
			"started_at": "1793560419.53", "cwd": "/home/u/Projects/x", "systemd_unit": "hermes-worker-proc_aaa.scope",
			"session_key": "agent:main:telegram:dm:720004689", "watcher_chat_id": "720004689", "parent_session_id": "sess_1"},
		{"session_id": "proc_bbb", "command": "python3 -m http.server\n 8000", "pid": 4002, "host_start_time": 888,
			"started_at": 1793560000.5},
		{"session_id": "proc_ccc", "command": "sleep 99", "pid": 4003, "host_start_time": 999, "started_at": 1000},
		{"session_id": "../etc", "command": "x", "pid": 4002},
	})
	e.write(filepath.Join(e.h.Runs[0], "live-1.stream.jsonl"), `{"type":"system","subtype":"init","cwd":"/elsewhere"}`+"\n")
	e.write(filepath.Join(e.h.Runs[0], "live-1-progress.md"), "10:00 spike done\n10:05 helper: 3 tests pass\n")
	long := strings.Repeat("é", 500)
	e.write(filepath.Join(e.h.Runs[0], "done-2.stream.jsonl"), fmt.Sprintf(stream, long)+`{"type":"assistant","message":{"content":[{"type":"text","text":"half a li`)
	e.write(filepath.Join(e.h.Runs[0], "old-3.stream.jsonl"), stream)
	old := now.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(e.h.Runs[0], "old-3.stream.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}

	_, raw := e.do("GET", "/agents")
	for _, s := range []string{"720004689", "telegram", "watcher", "session_key"} {
		if strings.Contains(raw, s) {
			t.Fatalf("list leaks %q: %s", s, raw)
		}
	}
	got := e.list()
	if len(got) != 4 {
		t.Fatalf("want 4 agents: %s", raw)
	}
	live := got["run_live-1"]
	if live.Kind != "delegate" || !live.Alive || live.Title != "live-1" || live.Cwd != "/home/u/Projects/x" ||
		live.StartedAt != 1793560419 || live.SessionID != "sess_1" || !live.Transcript || live.Progress != "10:05 helper: 3 tests pass" ||
		live.Outcome != "" || live.EndedAt != 0 {
		t.Fatalf("live run: %+v", live)
	}
	if p := got["proc_bbb"]; p.Kind != "process" || !p.Alive || p.Title != "python3 -m http.server 8000" || p.Transcript {
		t.Fatalf("process: %+v", p)
	}
	if p := got["proc_ccc"]; p.Alive {
		t.Fatalf("reused pid listed alive: %+v", p)
	}
	done := got["run_done-2"]
	if done.Alive || done.Outcome != "success" || done.NumTurns != 7 || done.DurationMS != 125000 || done.Cwd != "/home/u/Projects/repo-wt/issue-9" ||
		done.EndedAt == 0 || done.StartedAt != done.EndedAt-125 {
		t.Fatalf("finished run: %+v", done)
	}
	if i := []int{strings.Index(raw, "run_live-1"), strings.Index(raw, "proc_bbb"), strings.Index(raw, "run_done-2"),
		strings.Index(raw, "proc_ccc")}; !(i[0] < i[1] && i[1] < i[2] && i[2] < i[3]) {
		t.Fatalf("want live first, newest first: %s", raw)
	}

	st, b := e.do("GET", "/agents/run_done-2/transcript")
	var tr struct {
		Steps []Step
		Next  int
		Alive bool
	}
	if err := json.Unmarshal([]byte(b), &tr); st != 200 || err != nil {
		t.Fatalf("transcript: %d %s", st, b)
	}
	var lines []string
	for _, s := range tr.Steps {
		lines = append(lines, fmt.Sprintf("%d %s %v %s", s.Line, s.Kind, s.Error, s.Text))
	}
	want := []string{
		"3 text false Reading the spec.",
		"3 tool false Bash: go -C relay test ./...",
		"4 tool_result true " + strings.Repeat("é", 400) + "…",
		`5 tool false TodoWrite: {"todos":[{"content":"a"}]}`,
		"6 result false All done.",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") || tr.Next != 6 || tr.Alive {
		t.Fatalf("steps:\n%s\nnext %d", strings.Join(lines, "\n"), tr.Next)
	}
	if st, b := e.do("GET", "/agents/run_done-2/transcript?after=6"); st != 200 || b != `{"alive":false,"next":6,"steps":[]}` {
		t.Fatalf("nothing new: %d %s", st, b)
	}
	if st, b := e.do("GET", "/agents/run_done-2/transcript?after=4"); st != 200 || !strings.Contains(b, `"line":5`) || strings.Contains(b, `"line":4`) {
		t.Fatalf("after 4: %d %s", st, b)
	}
	if st, b := e.do("GET", "/agents/proc_bbb/transcript"); st != 200 || b != `{"alive":true,"next":0,"steps":[]}` {
		t.Fatalf("process transcript: %d %s", st, b)
	}
	for _, c := range []struct {
		path string
		st   int
	}{
		{"/agents/run_old-3/transcript", 404},
		{"/agents/run_nope/transcript", 404},
		{"/agents/run_..%2Fx/transcript", 404},
		{"/agents/run_done-2/transcript?after=-1", 400},
		{"/agents/run_done-2/transcript?after=x", 400},
	} {
		if st, b := e.do("GET", c.path); st != c.st {
			t.Errorf("%s: %d %s", c.path, st, b)
		}
	}

	// Stop: only live, listed processes; with a scope, through systemctl (injected here).
	if st, _ := e.do("POST", "/agents/proc_ccc/stop"); st != 404 {
		t.Fatalf("stopping a reused pid: %d", st)
	}
	if st, _ := e.do("POST", "/agents/run_done-2/stop"); st != 404 {
		t.Fatalf("stopping a finished run: %d", st)
	}
	if st, b := e.do("POST", "/agents/run_live-1/stop"); st != 202 || b != `{"stopping":true}` {
		t.Fatalf("stop: %d %s", st, b)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		e.mu.Lock()
		u := strings.Join(e.unit, ",")
		e.mu.Unlock()
		if u == "hermes-worker-proc_aaa.scope" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("systemctl stop: %q", u)
		}
	}
}

// child starts a shell script in its own process group, waits for its first output line (it is set up) and lists
// it in processes.json as Hermes would.
func (e *env) child(id, script string) (pid int, exited chan struct{}) {
	e.t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
		e.t.Fatal(err)
	}
	args := []string{"sh", "-c", script}
	exited = make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	e.t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-exited })
	var start int64
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var ok bool
		if start, ok = e.h.startTime(int64(cmd.Process.Pid)); ok || time.Now().After(deadline) {
			break
		}
	}
	e.processes([]map[string]any{{"session_id": id, "command": strings.Join(args, " "), "pid": cmd.Process.Pid,
		"host_start_time": start, "started_at": time.Now().Unix()}})
	return cmd.Process.Pid, exited
}

func waitExit(t *testing.T, exited chan struct{}, within time.Duration) {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(within):
		t.Fatal("process still running")
	}
}

func TestStopSignals(t *testing.T) {
	e := newEnv(t, "/proc")
	_, exited := e.child("proc_sleep", "echo ready; sleep 60")
	if a := e.list()["proc_sleep"]; !a.Alive {
		t.Fatalf("child not alive: %+v", a)
	}
	if st, b := e.do("POST", "/agents/proc_sleep/stop"); st != 202 {
		t.Fatalf("stop: %d %s", st, b)
	}
	waitExit(t, exited, 2*time.Second)

	// A process that ignores SIGTERM gets SIGKILL after the grace period.
	_, exited = e.child("proc_stubborn", `trap "" TERM; echo ready; sleep 60; true`)
	started := time.Now()
	if st, b := e.do("POST", "/agents/proc_stubborn/stop"); st != 202 {
		t.Fatalf("stop: %d %s", st, b)
	}
	waitExit(t, exited, 3*time.Second)
	if d := time.Since(started); d < e.h.Grace {
		t.Fatalf("killed after %v, before the grace period", d)
	}
	if len(e.unit) != 0 {
		t.Fatalf("systemctl used without a unit: %v", e.unit)
	}
}

const runJSON = `{"id":"run_%[1]s","repo":"xMuramasa/trismegistus","issue":%[2]d,"title":"Agents helper:\n read runs","model":"sonnet",` +
	`"effort":"high","started_at":"2026-11-02T18:32:27+00:00","unit":"dispatch-%[3]s.service"}`

func TestDispatchRuns(t *testing.T) {
	e := newEnv(t, "")
	hermes, scratch := e.h.Runs[0], e.h.Runs[1]
	// A live dispatch run (unit active, not in the process list), its finished first attempt (whose run.json still
	// names the live unit), a run.json whose stream isn't there yet, a hand-launched run in the old scratch folder, a
	// run in scratch without run.json, and broken run.json files.
	e.write(filepath.Join(hermes, "trismegistus-issue-27.stream.jsonl"), `{"type":"system","subtype":"init","cwd":"/wt"}`+"\n")
	e.write(filepath.Join(hermes, "trismegistus-issue-27.run.json"), fmt.Sprintf(runJSON, "trismegistus-issue-27", 27, "trismegistus-issue-27"))
	e.write(filepath.Join(hermes, "trismegistus-issue-27-attempt1.stream.jsonl"), fmt.Sprintf(stream, "x"))
	e.write(filepath.Join(hermes, "trismegistus-issue-27-attempt1.run.json"), fmt.Sprintf(runJSON, "trismegistus-issue-27", 27, "trismegistus-issue-27"))
	e.write(filepath.Join(hermes, "trismegistus-mac-issue-48.run.json"), fmt.Sprintf(runJSON, "trismegistus-mac-issue-48", 48, "trismegistus-mac-issue-48"))
	e.write(filepath.Join(scratch, "hand-8.stream.jsonl"), stream)
	e.write(filepath.Join(scratch, "hand-8-launch.sh"), "#!/bin/sh\n")
	e.write(filepath.Join(scratch, "plain-9.stream.jsonl"), stream)
	e.write(filepath.Join(hermes, "broken-10.stream.jsonl"), stream)
	e.write(filepath.Join(hermes, "broken-10.run.json"), `{"repo": "xMuramasa/x", "issue": `)
	e.write(filepath.Join(hermes, "evil-11.stream.jsonl"), stream)
	e.write(filepath.Join(hermes, "evil-11.run.json"), `{"repo":"../../etc","issue":-3,"unit":"dispatch-other.service"}`)
	e.write(filepath.Join(hermes, "bad name.stream.jsonl"), stream)
	e.write(filepath.Join(hermes, ".hidden.run.json"), `{}`)
	e.write(filepath.Join(e.h.Proc, "4001", "stat"), fakeStat(4001, "S", 777))
	e.processes([]map[string]any{{"session_id": "proc_hand", "command": "bash " + filepath.Join(scratch, "hand-8-launch.sh"),
		"pid": 4001, "host_start_time": 777, "started_at": 1793560419}})
	e.active = map[string]bool{"dispatch-trismegistus-issue-27.service": true, "dispatch-evil-11.service": true,
		"dispatch-other.service": true, "dispatch-trismegistus-mac-issue-48.service": true}

	_, raw := e.do("GET", "/agents")
	idRe := regexp.MustCompile(`^(proc_[A-Za-z0-9]{1,64}|run_[A-Za-z0-9][A-Za-z0-9._-]{0,63})$`) // the relay's agentIDRe
	got := e.list()
	for id := range got {
		if !idRe.MatchString(id) {
			t.Errorf("id %q is outside the relay's agentIDRe", id)
		}
	}
	if len(got) != 7 {
		t.Fatalf("want 7 agents: %s", raw)
	}
	live := got["run_trismegistus-issue-27"]
	if !live.Alive || live.Repo != "xMuramasa/trismegistus" || live.Issue != 27 || live.IssueTitle != "Agents helper: read runs" ||
		live.Model != "sonnet" || live.StartedAt != 1793644347 || live.Title != "trismegistus-issue-27" || live.Cwd != "/wt" {
		t.Fatalf("live dispatch run: %+v", live)
	}
	if a := got["run_trismegistus-issue-27-attempt1"]; a.Alive || a.Issue != 27 || a.Outcome != "success" || a.EndedAt == 0 || a.StartedAt != 1793644347 {
		t.Fatalf("earlier attempt: %+v", a)
	}
	if a := got["run_trismegistus-mac-issue-48"]; !a.Alive || a.Issue != 48 || a.Transcript || a.StartedAt == 0 {
		t.Fatalf("run.json without a stream: %+v", a)
	}
	if a := got["run_hand-8"]; !a.Alive || a.Kind != "delegate" || a.Repo != "" || a.Issue != 0 || !a.Transcript {
		t.Fatalf("hand-launched run from the scratch folder: %+v", a)
	}
	for _, id := range []string{"run_plain-9", "run_broken-10", "run_evil-11"} {
		if a, ok := got[id]; !ok || a.Alive || a.Repo != "" || a.Issue != 0 || a.IssueTitle != "" && id != "run_evil-11" || !a.Transcript || a.Outcome != "success" {
			t.Fatalf("%s: %+v", id, a)
		}
	}
	if strings.Contains(raw, `"repo":""`) || strings.Contains(raw, `"issue":0`) || strings.Contains(raw, "etc") {
		t.Fatalf("absent run info must be omitted: %s", raw)
	}
	if st, b := e.do("GET", "/agents/run_hand-8/transcript"); st != 200 || !strings.Contains(b, `"kind":"result"`) {
		t.Fatalf("transcript from the scratch folder: %d %s", st, b)
	}

	// Stop goes through the unit, for the run that is alive; the attempt that names the same unit is finished.
	if st, _ := e.do("POST", "/agents/run_trismegistus-issue-27-attempt1/stop"); st != 404 {
		t.Fatalf("stopping a finished attempt: %d", st)
	}
	if st, _ := e.do("POST", "/agents/run_trismegistus-issue-27/stop"); st != 202 {
		t.Fatalf("stop: %d", st)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		e.mu.Lock()
		u := strings.Join(e.unit, ",")
		e.mu.Unlock()
		if u == "dispatch-trismegistus-issue-27.service" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("systemctl stop: %q", u)
		}
	}

	// The unit is gone (the run ended): finished, listed with what it left.
	e.mu.Lock()
	e.active = nil
	e.mu.Unlock()
	if a := e.list()["run_trismegistus-issue-27"]; a.Alive || a.Issue != 27 {
		t.Fatalf("ended run: %+v", a)
	}
}

// waitUnits waits until StopUnit has been called with exactly these units.
func (e *env) waitUnits(want string) {
	e.t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		e.mu.Lock()
		u := strings.Join(e.unit, ",")
		e.mu.Unlock()
		if u == want {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("systemctl stop: %q, want %q", u, want)
		}
	}
}

const caduceusJSON = `{"id":"run_%[1]s","repo":"xMuramasa/caduceus","issue":19,"title":"Core 4","model":"opus","effort":"high",` +
	`"started_at":"%[2]s","unit":%[3]s}`

func TestCaduceusRuns(t *testing.T) {
	e := newEnv(t, "")
	dir := e.h.Runs[0]
	// A live Caduceus run, and runs whose run.json names a unit other than their own (both active), or none.
	e.write(filepath.Join(dir, "caduceus-issue-19.run.json"), fmt.Sprintf(caduceusJSON, "caduceus-issue-19", "2026-11-23T13:53:07+00:00", `"caduceus-caduceus-issue-19.service"`))
	e.write(filepath.Join(dir, "other-1.run.json"), fmt.Sprintf(caduceusJSON, "other-1", "2026-11-23T13:53:07+00:00", `"caduceus-other.service"`))
	e.write(filepath.Join(dir, "evil-2.run.json"), fmt.Sprintf(caduceusJSON, "evil-2", "2026-11-23T13:53:07+00:00", `"evil.service"`))
	e.write(filepath.Join(dir, "archived-3.run.json"), fmt.Sprintf(caduceusJSON, "archived-3", "2026-11-23T13:53:07+00:00", `null`))
	e.active = map[string]bool{"caduceus-caduceus-issue-19.service": true, "caduceus-other.service": true, "evil.service": true}

	got := e.list()
	if a := got["run_caduceus-issue-19"]; !a.Alive || a.Repo != "xMuramasa/caduceus" || a.Issue != 19 || a.StartedAt != 1795441987 {
		t.Fatalf("live Caduceus run: %+v", a)
	}
	for _, id := range []string{"run_other-1", "run_evil-2", "run_archived-3"} {
		if a, ok := got[id]; !ok || a.Alive {
			t.Fatalf("%s: %+v", id, a)
		}
		if st, _ := e.do("POST", "/agents/"+id+"/stop"); st != 404 {
			t.Fatalf("%s stopped: %d", id, st)
		}
	}
	if st, _ := e.do("POST", "/agents/run_caduceus-issue-19/stop"); st != 202 {
		t.Fatalf("stop: %d", st)
	}
	e.waitUnits("caduceus-caduceus-issue-19.service")
}

func TestSameRunInTwoFolders(t *testing.T) {
	e := newEnv(t, "")
	first, second := e.h.Runs[0], e.h.Runs[1]
	hourAgo := time.Now().Add(-time.Hour)
	run := func(dir, name, startedAt, unit, from string) {
		e.write(filepath.Join(dir, name+".run.json"), fmt.Sprintf(caduceusJSON, name, startedAt, `"`+unit+`"`))
		e.write(filepath.Join(dir, name+".stream.jsonl"), `{"type":"system","subtype":"init","cwd":"/`+from+`"}`+"\n"+
			`{"type":"result","subtype":"success","num_turns":1,"duration_ms":1000,"result":"from `+from+`"}`+"\n")
		e.write(filepath.Join(dir, name+"-progress.md"), "progress "+from+"\n")
	}
	// The second folder's run started later; the first's files were written later (started_at wins over mtime).
	run(first, "a-issue-1", "2026-11-22T10:00:00+00:00", "dispatch-a-issue-1.service", "dispatch")
	run(second, "a-issue-1", "2026-11-23T10:00:00+00:00", "caduceus-a-issue-1.service", "caduceus")
	// The first folder's run started later, its files written earlier.
	run(first, "b-issue-2", "2026-11-23T10:00:00+00:00", "caduceus-b-issue-2.service", "caduceus")
	run(second, "b-issue-2", "2026-11-22T10:00:00+00:00", "dispatch-b-issue-2.service", "dispatch")
	// No run.json in the first folder: the newest run files win, here the second folder's.
	e.write(filepath.Join(first, "c-issue-3.stream.jsonl"), `{"type":"result","subtype":"success","result":"from dispatch"}`+"\n")
	run(second, "c-issue-3", "2026-11-23T10:00:00+00:00", "caduceus-c-issue-3.service", "caduceus")
	for _, f := range []string{"a-issue-1.run.json", "a-issue-1.stream.jsonl", "a-issue-1-progress.md"} {
		if err := os.Chtimes(filepath.Join(second, f), hourAgo, hourAgo); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"b-issue-2.run.json", "b-issue-2.stream.jsonl", "b-issue-2-progress.md", "c-issue-3.stream.jsonl"} {
		if err := os.Chtimes(filepath.Join(first, f), hourAgo, hourAgo); err != nil {
			t.Fatal(err)
		}
	}
	e.active = map[string]bool{"dispatch-a-issue-1.service": true, "caduceus-a-issue-1.service": true,
		"caduceus-b-issue-2.service": true, "dispatch-b-issue-2.service": true}

	_, raw := e.do("GET", "/agents")
	got := e.list()
	for _, name := range []string{"a-issue-1", "b-issue-2", "c-issue-3"} {
		if n := strings.Count(raw, `"id":"run_`+name+`"`); n != 1 {
			t.Fatalf("%s listed %d times: %s", name, n, raw)
		}
		a := got["run_"+name]
		if a.Cwd != "/caduceus" || a.Progress != "progress caduceus" || a.StartedAt != 1795428000 {
			t.Fatalf("%s not from the newer folder: %+v", name, a)
		}
		if _, b := e.do("GET", "/agents/run_"+name+"/transcript"); !strings.Contains(b, "from caduceus") || strings.Contains(b, "from dispatch") {
			t.Fatalf("%s transcript not from the newer folder: %s", name, b)
		}
	}
	if !got["run_a-issue-1"].Alive || !got["run_b-issue-2"].Alive {
		t.Fatalf("runs from the newer folder not alive: %s", raw)
	}
	if st, _ := e.do("POST", "/agents/run_a-issue-1/stop"); st != 202 {
		t.Fatalf("stop: %d", st)
	}
	e.waitUnits("caduceus-a-issue-1.service")
}

func TestParseActiveUnits(t *testing.T) {
	out := "dispatch-a-issue-1.service loaded active running a\ndispatch-b.service loaded activating start b\n" +
		"dispatch-c.service loaded failed failed c\n\n" +
		"caduceus-probe-issue-0.service loaded active running [systemd-run] /usr/bin/sleep 20\n" +
		"dispatch-probe-issue-0.service loaded active running [systemd-run] /usr/bin/sleep 20\n" +
		"caduceus-d.service loaded inactive dead d\n"
	if got := parseActiveUnits(out); len(got) != 4 || !got["dispatch-a-issue-1.service"] || !got["dispatch-b.service"] ||
		!got["caduceus-probe-issue-0.service"] || !got["dispatch-probe-issue-0.service"] {
		t.Fatalf("%v", got)
	}
}

func TestListenSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents", "agents.sock")
	ln, err := listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o666 {
		t.Fatalf("socket mode: %v %v", fi, err)
	}
	if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o755 {
		t.Fatalf("dir mode: %v", di.Mode())
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := listen("relative.sock"); err == nil {
		t.Fatal("relative path accepted")
	}
}
