package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	finishedFor  = 24 * time.Hour // finished delegate runs are listed this long (Hermes prunes their files after ~a day)
	maxSteps     = 200            // transcript steps per answer; the app asks again with the next `after`
	resultChars  = 400            // tool results are cut to this
	finalChars   = 8000           // the final result is cut to this
	summaryChars = 160            // one-line tool input summaries
)

var (
	procIDRe  = regexp.MustCompile(`^proc_[A-Za-z0-9]{1,64}$`)
	runNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	unitRe    = regexp.MustCompile(`^hermes-worker-proc_[A-Za-z0-9]{1,64}\.scope$`)
)

// Helper answers for the background work Hermes knows about. It reads only processes.json, /proc/<pid>/stat and
// the stream, progress and result files of delegate runs in Runs, and images /media is asked for (media.go: under
// Home, by strict rules); never environment variables or other files.
type Helper struct {
	Processes string                  // Hermes' processes.json
	Runs      string                  // directory of Claude Code delegate runs (<name>-launch.sh, <name>.stream.jsonl...)
	Proc      string                  // /proc
	Home      string                  // expands ~ in commands
	Grace     time.Duration           // SIGTERM → SIGKILL
	StopUnit  func(unit string) error // systemctl --user stop (injected so tests never call systemctl)
	Now       func() time.Time
	Log       *slog.Logger
}

// Agent is one piece of background work as the app sees it.
type Agent struct {
	ID         string `json:"id"`   // proc_… (a Hermes process) or run_<name> (a delegate run, alive or finished)
	Kind       string `json:"kind"` // process or delegate
	Title      string `json:"title"`
	Command    string `json:"command,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"` // unix seconds
	EndedAt    int64  `json:"ended_at,omitempty"`   // finished delegate runs: when the transcript was last written
	Alive      bool   `json:"alive"`
	SessionID  string `json:"session_id,omitempty"` // the Hermes conversation that started it, when Hermes knows it
	Transcript bool   `json:"transcript"`
	Progress   string `json:"progress,omitempty"`    // delegate runs: last line of <name>-progress.md
	Outcome    string `json:"outcome,omitempty"`     // delegate runs: the result line's subtype (success, error_…)
	NumTurns   int    `json:"num_turns,omitempty"`   // from the result line
	DurationMS int64  `json:"duration_ms,omitempty"` // from the result line

	pid, start int64
	unit       string
}

// entry is the part of a processes.json record the helper uses; the rest (session_key, task ids, watcher_*) is never
// read into memory.
type entry struct {
	SessionID     string  `json:"session_id"`
	Command       string  `json:"command"`
	PID           flexNum `json:"pid"`
	HostStartTime flexNum `json:"host_start_time"`
	SystemdUnit   string  `json:"systemd_unit"`
	Cwd           string  `json:"cwd"`
	StartedAt     flexNum `json:"started_at"`
	ParentSession string  `json:"parent_session_id"`
}

// flexNum reads a number written either as a JSON number or as a string.
type flexNum float64

func (n *flexNum) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	*n = flexNum(f)
	return err
}

// List returns the running processes, then delegate runs that finished within finishedFor, newest first.
func (h *Helper) List() ([]Agent, error) {
	var entries []entry
	b, err := os.ReadFile(h.Processes)
	if err == nil {
		err = json.Unmarshal(b, &entries)
	} else if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	var out []Agent
	seen := map[string]bool{}
	for _, e := range entries {
		if !procIDRe.MatchString(e.SessionID) {
			continue
		}
		a := Agent{ID: e.SessionID, Kind: "process", Title: oneLine(e.Command, 80), Command: e.Command, Cwd: e.Cwd,
			StartedAt: int64(e.StartedAt), SessionID: e.ParentSession, pid: int64(e.PID), start: int64(e.HostStartTime),
			unit: e.SystemdUnit}
		st, ok := h.startTime(a.pid)
		a.Alive = ok && (a.start == 0 || st == a.start) // not alive: gone, or the pid now names another process
		if name, ok := h.delegateName(e.Command); ok && !seen[name] {
			seen[name] = true
			a.ID, a.Kind, a.Title = "run_"+name, "delegate", name
			h.describeRun(&a, name)
		}
		out = append(out, a)
	}
	files, _ := filepath.Glob(filepath.Join(h.Runs, "*.stream.jsonl"))
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".stream.jsonl")
		if seen[name] || !runNameRe.MatchString(name) {
			continue
		}
		if fi, err := os.Stat(f); err != nil || h.Now().Sub(fi.ModTime()) > finishedFor {
			continue
		}
		a := Agent{ID: "run_" + name, Kind: "delegate", Title: name}
		h.describeRun(&a, name)
		out = append(out, a)
	}
	slices.SortStableFunc(out, func(x, y Agent) int {
		if x.Alive != y.Alive {
			if x.Alive {
				return -1
			}
			return 1
		}
		return int(y.StartedAt - x.StartedAt)
	})
	return out, nil
}

func (h *Helper) find(id string) (Agent, bool) {
	list, err := h.List()
	if err != nil {
		return Agent{}, false
	}
	for _, a := range list {
		if a.ID == id {
			return a, true
		}
	}
	return Agent{}, false
}

// delegateName recognizes `bash <Runs>/<name>-launch.sh`, the way Hermes starts Claude Code delegate runs.
func (h *Helper) delegateName(command string) (string, bool) {
	f := strings.Fields(command)
	if len(f) != 2 || f[0] != "bash" {
		return "", false
	}
	p := f[1]
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		p = filepath.Join(h.Home, rest)
	}
	name, ok := strings.CutSuffix(filepath.Base(p), "-launch.sh")
	return name, ok && filepath.Dir(p) == filepath.Clean(h.Runs) && runNameRe.MatchString(name)
}

func (h *Helper) runFile(name, suffix string) string { return filepath.Join(h.Runs, name+suffix) }

// describeRun fills in a delegate run from its files: cwd from the init line, outcome from the result line, the last
// progress line.
func (h *Helper) describeRun(a *Agent, name string) {
	stream := h.runFile(name, ".stream.jsonl")
	fi, err := os.Stat(stream)
	a.Transcript = err == nil
	if err != nil {
		return
	}
	if a.Cwd == "" {
		a.Cwd = initCwd(stream)
	}
	b := tail(stream, 256<<10)                              // the result line, with its final answer and usage, fits in this
	b = bytes.TrimSpace(b[:bytes.LastIndexByte(b, '\n')+1]) // complete lines only
	var m streamLine
	if json.Unmarshal(b[bytes.LastIndexByte(b, '\n')+1:], &m) == nil && m.Type == "result" {
		a.Outcome, a.NumTurns, a.DurationMS = m.Subtype, m.NumTurns, m.DurationMS
	}
	if !a.Alive && a.StartedAt == 0 && a.DurationMS > 0 {
		a.StartedAt = fi.ModTime().Unix() - a.DurationMS/1000
	}
	if !a.Alive && a.EndedAt == 0 {
		a.EndedAt = fi.ModTime().Unix()
	}
	if lines := strings.Split(strings.TrimSpace(string(tail(h.runFile(name, "-progress.md"), 4096))), "\n"); len(lines) > 0 {
		a.Progress = oneLine(lines[len(lines)-1], 300)
	}
}

// initCwd reads the cwd of Claude Code's init line, a few lines into the stream.
func initCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for range 20 {
		line, err := r.ReadBytes('\n')
		var m struct{ Type, Subtype, Cwd string }
		if json.Unmarshal(line, &m) == nil && m.Type == "system" && m.Subtype == "init" {
			return m.Cwd
		}
		if err != nil {
			return ""
		}
	}
	return ""
}

// tail reads at most the last n bytes of a file.
func tail(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > n {
		_, _ = f.Seek(-n, io.SeekEnd)
	}
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return b
}

// startTime reads field 22 of /proc/<pid>/stat (start time in clock ticks); ok is false for a missing or zombie pid.
func (h *Helper) startTime(pid int64) (int64, bool) {
	if pid <= 1 {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join(h.Proc, strconv.FormatInt(pid, 10), "stat"))
	if err != nil {
		return 0, false
	}
	i := bytes.LastIndexByte(b, ')') // the command name may hold spaces and parentheses
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(b[i+1:])) // f[0] is field 3 (state)
	if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
		return 0, false
	}
	st, err := strconv.ParseInt(f[19], 10, 64)
	return st, err == nil
}

// running is true while pid is alive and still the process that was started at start.
func (h *Helper) running(pid, start int64) bool {
	st, ok := h.startTime(pid)
	return ok && st == start
}

// Step is one readable transcript step.
type Step struct {
	Line  int    `json:"line"`
	Kind  string `json:"kind"` // text, tool, tool_result, result
	Text  string `json:"text"`
	Error bool   `json:"error,omitempty"`
}

type streamLine struct {
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
	Result     string `json:"result"`
	IsError    bool   `json:"is_error"`
	NumTurns   int    `json:"num_turns"`
	DurationMS int64  `json:"duration_ms"`
	Message    struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// Transcript returns the readable steps of a delegate run after line `after` (1-based lines of the stream), and the
// `after` for the next call. Only complete lines count, so a line being written is read next time.
func (h *Helper) Transcript(a Agent, after int) ([]Step, int, error) {
	steps := []Step{}
	if a.Kind != "delegate" || !a.Transcript {
		return steps, after, nil // Hermes keeps other processes' output in its own memory only
	}
	f, err := os.Open(h.runFile(strings.TrimPrefix(a.ID, "run_"), ".stream.jsonl"))
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	// shortcut: rescans the file from the start on every call; fine for runs of a few MB, use a byte offset if slow.
	r := bufio.NewReader(f)
	n := 0
	for len(steps) < maxSteps {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // EOF, or a line still being written
		}
		n++
		if n > after {
			steps = append(steps, parseLine(n, line)...)
		}
	}
	return steps, max(n, after), nil
}

func parseLine(n int, line []byte) []Step {
	var m streamLine
	if json.Unmarshal(line, &m) != nil {
		return nil
	}
	var out []Step
	switch m.Type {
	case "assistant", "user":
		var parts []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			Content json.RawMessage `json:"content"`
			IsError bool            `json:"is_error"`
		}
		_ = json.Unmarshal(m.Message.Content, &parts) // a plain string (the prompt) has no steps
		for _, p := range parts {
			switch {
			case p.Type == "text" && m.Type == "assistant" && strings.TrimSpace(p.Text) != "":
				out = append(out, Step{Line: n, Kind: "text", Text: p.Text})
			case p.Type == "tool_use":
				out = append(out, Step{Line: n, Kind: "tool", Text: p.Name + ": " + inputSummary(p.Input)})
			case p.Type == "tool_result":
				out = append(out, Step{Line: n, Kind: "tool_result", Text: cut(contentText(p.Content), resultChars), Error: p.IsError})
			}
		}
	case "result":
		out = append(out, Step{Line: n, Kind: "result", Text: cut(m.Result, finalChars), Error: m.IsError})
	}
	return out
}

// inputSummary is one line about a tool call: its main argument, or its input as compact JSON.
func inputSummary(raw json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "description", "skill", "prompt"} {
		if s, ok := in[k].(string); ok && s != "" {
			return oneLine(s, summaryChars)
		}
	}
	return oneLine(string(raw), summaryChars)
}

// contentText reads a tool result's content: a string or a list of {type: text, text}.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct{ Type, Text string }
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func oneLine(s string, n int) string { return cut(strings.Join(strings.Fields(s), " "), n) }

// Stop ends a listed, live process: its systemd scope if Hermes made one, else SIGTERM to its process group and
// SIGKILL after Grace. It returns at once; the stop finishes in the background.
func (h *Helper) Stop(a Agent) bool {
	if !a.Alive || a.start == 0 || a.pid <= 1 || a.pid == int64(os.Getpid()) || !h.running(a.pid, a.start) {
		return false
	}
	go func() {
		if unitRe.MatchString(a.unit) {
			err := h.StopUnit(a.unit)
			if err == nil {
				return
			}
			h.Log.Warn("systemctl stop failed, signalling instead", "id", a.ID, "err", err.Error())
		}
		h.kill(a.pid, a.start)
	}()
	return true
}

func (h *Helper) kill(pid, start int64) {
	p := int(pid)
	target := p
	if pg, err := syscall.Getpgid(p); err == nil && pg == p && pg != syscall.Getpgrp() {
		target = -p // its own process group: take the children (claude under the launch script) too
	}
	if !h.running(pid, start) {
		return
	}
	_ = syscall.Kill(target, syscall.SIGTERM)
	for deadline := time.Now().Add(h.Grace); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !h.running(pid, start) {
			return
		}
	}
	if h.running(pid, start) {
		_ = syscall.Kill(target, syscall.SIGKILL)
	}
}

// Handler serves GET /agents, GET /agents/{id}/transcript?after=N, POST /agents/{id}/stop and GET /media?path=….
func (h *Helper) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /media", h.serveMedia)
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		list, err := h.List()
		if err != nil {
			h.Log.Error("reading processes failed", "err", err.Error())
			writeErr(w, http.StatusInternalServerError, "internal", "could not read Hermes' process list")
			return
		}
		if list == nil {
			list = []Agent{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"agents": list})
	})
	mux.HandleFunc("GET /agents/{id}/transcript", func(w http.ResponseWriter, r *http.Request) {
		after, err := strconv.Atoi(r.URL.Query().Get("after"))
		if r.URL.Query().Get("after") == "" {
			after, err = 0, nil
		}
		if err != nil || after < 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "after must be a line number ≥ 0")
			return
		}
		a, ok := h.find(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "not_found", "no such agent")
			return
		}
		steps, next, err := h.Transcript(a, after)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "the transcript is gone")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"steps": steps, "next": next, "alive": a.Alive})
	})
	mux.HandleFunc("POST /agents/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		a, ok := h.find(r.PathValue("id"))
		if !ok || !h.Stop(a) {
			writeErr(w, http.StatusNotFound, "not_found", "no such running agent")
			return
		}
		h.Log.Info("stopping", "id", a.ID, "pid", a.pid, "unit", a.unit)
		writeJSON(w, http.StatusAccepted, map[string]bool{"stopping": true})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
