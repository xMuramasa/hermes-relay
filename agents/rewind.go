package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// rewindScript runs inside Hermes' interpreter; see rewind.py.
//
//go:embed rewind.py
var rewindScript string

// exitNotUserMessage is rewind.py's exit code for a message id that is not one of the session's user messages.
const exitNotUserMessage = 3

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

var errNotUserMessage = errors.New("not one of the session's user messages")

// rewound is rewind.py's answer, passed on to the relay.
type rewound struct {
	Rewound int `json:"rewound"`
	Turns   int `json:"turns"`
}

// serveRewind answers POST /sessions/{id}/rewind {"before_message_id": n}: Hermes soft-deletes the user message n
// and everything after it (SessionDB.rewind_user_turn).
func (h *Helper) serveRewind(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	var body struct {
		Before int64 `json:"before_message_id"`
	}
	if !sessionIDRe.MatchString(sid) {
		writeErr(w, http.StatusBadRequest, "bad_request", "bad session id")
		return
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.Before < 1 {
		writeErr(w, http.StatusBadRequest, "bad_request", "before_message_id must be a positive integer")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.RewindTimeout)
	defer cancel()
	out, err := h.rewind(ctx, sid, body.Before)
	switch {
	case errors.Is(err, errNotUserMessage):
		h.Log.Info("rewind refused", "session_id", sid, "before_message_id", body.Before, "reason", "not a user message")
		writeErr(w, http.StatusNotFound, "not_a_user_message", "not one of the session's user messages")
	case err != nil:
		h.Log.Warn("rewind failed", "session_id", sid, "before_message_id", body.Before, "err", err.Error())
		writeErr(w, http.StatusBadGateway, "rewind_failed", err.Error())
	default:
		h.Log.Info("rewound", "session_id", sid, "before_message_id", body.Before, "rewound", out.Rewound, "turns", out.Turns)
		writeJSON(w, http.StatusOK, out)
	}
}

// rewind asks Hermes for its interpreter (it moves with Hermes updates, so every time) and runs rewind.py in it.
// Error messages are short and never carry message text: the script prints none.
func (h *Helper) rewind(ctx context.Context, sid string, before int64) (rewound, error) {
	env := append(os.Environ(), "HERMES_HOME="+h.HermesHome)
	rt := exec.CommandContext(ctx, h.Hermes, "--print-runtime-command")
	rt.Env, rt.WaitDelay = env, time.Second
	b, err := rt.Output()
	if err != nil {
		return rewound{}, failure(ctx, "hermes --print-runtime-command: "+err.Error())
	}
	var argv []string
	if err := json.Unmarshal(b, &argv); err != nil || len(argv) == 0 || argv[0] == "" {
		return rewound{}, errors.New("hermes printed no runtime command")
	}
	agent, _ := json.Marshal(h.HermesAgent) // a JSON string is a valid Python string literal
	code := "import sys; sys.path.insert(0, " + string(agent) + "); import hermes_bootstrap\n" + rewindScript
	cmd := exec.CommandContext(ctx, argv[0], "-I", "-c", code, sid, strconv.FormatInt(before, 10))
	var stderr bytes.Buffer
	cmd.Env, cmd.Stderr, cmd.WaitDelay = env, &stderr, time.Second
	b, err = cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == exitNotUserMessage {
		return rewound{}, errNotUserMessage
	}
	if err != nil {
		reason := lastLine(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		return rewound{}, failure(ctx, reason)
	}
	var out rewound
	if err := json.Unmarshal([]byte(lastLine(string(b))), &out); err != nil {
		return rewound{}, errors.New("the rewind script printed no result")
	}
	return out, nil
}

// failure is "timed out" or the reason, at most 200 characters.
func failure(ctx context.Context, reason string) error {
	if ctx.Err() != nil {
		return errors.New("timed out")
	}
	return errors.New(cut(reason, 200))
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
