package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHermes stands in for hermes and its Python: hermes prints the fake python's path; the python records its argv
// and HERMES_HOME, then acts on the message id (its last argument).
func fakeHermes(t *testing.T, e *env) (argvFile string) {
	t.Helper()
	dir := t.TempDir()
	py := filepath.Join(dir, "python3")
	argvFile = filepath.Join(dir, "argv")
	e.write(filepath.Join(dir, "hermes"), "#!/bin/sh\n[ \"$1\" = --print-runtime-command ] || exit 9\necho '[\""+py+"\", \"-m\", \"hermes\"]'\n")
	e.write(py, `#!/bin/sh
printf '%s\0' "$@" > `+argvFile+`
printf '%s' "$HERMES_HOME" > `+argvFile+`.home
eval "last=\${$#}"
case "$last" in
7) echo 'some import chatter'; echo '{"rewound": 10, "turns": 2}' ;;
8) exit 3 ;;
9) echo 'Traceback (most recent call last):' >&2; echo 'sqlite3.OperationalError: database is locked' >&2; exit 1 ;;
10) exec sleep 5 ;;
11) echo 'not json' ;;
esac
`)
	for _, f := range []string{"hermes", "python3"} {
		if err := os.Chmod(filepath.Join(dir, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.h.Hermes, e.h.HermesHome, e.h.HermesAgent = filepath.Join(dir, "hermes"), "/fake/hermes-home", "/fake/hermes-agent"
	e.h.RewindTimeout = 500 * time.Millisecond
	return argvFile
}

func (e *env) rewind(sid, body string) (int, string) {
	e.t.Helper()
	resp, err := http.Post(e.srv.URL+"/sessions/"+sid+"/rewind", "application/json", strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRewind(t *testing.T) {
	e := newEnv(t, "")
	argvFile := fakeHermes(t, e)

	st, body := e.rewind("api_1794362337_d0dd6397", `{"before_message_id": 7}`)
	if st != 200 || body != `{"rewound":10,"turns":2}` {
		t.Fatalf("rewind: %d %s", st, body)
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	wantCode := "import sys; sys.path.insert(0, \"/fake/hermes-agent\"); import hermes_bootstrap\n" + rewindScript
	if len(argv) != 5 || argv[0] != "-I" || argv[1] != "-c" || argv[2] != wantCode || argv[3] != "api_1794362337_d0dd6397" || argv[4] != "7" {
		t.Fatalf("argv %q", argv)
	}
	if home, _ := os.ReadFile(argvFile + ".home"); string(home) != "/fake/hermes-home" {
		t.Fatalf("HERMES_HOME %q", home)
	}

	for name, c := range map[string]struct {
		id   string
		st   int
		code string
	}{
		"not a user message": {"8", 404, `"not_a_user_message"`},
		"script failure":     {"9", 502, `"sqlite3.OperationalError: database is locked"`},
		"timeout":            {"10", 502, `"timed out"`},
		"no result":          {"11", 502, `"the rewind script printed no result"`},
	} {
		start := time.Now()
		if st, body := e.rewind("s1", `{"before_message_id": `+c.id+`}`); st != c.st || !strings.Contains(body, c.code) {
			t.Errorf("%s: %d %s", name, st, body)
		}
		if time.Since(start) > 3*time.Second {
			t.Errorf("%s took %v", name, time.Since(start))
		}
	}

	if err := os.Remove(argvFile); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"bad session id":    {"bad%20id", `{"before_message_id": 7}`},
		"long session id":   {strings.Repeat("a", 129), `{"before_message_id": 7}`},
		"zero":              {"s1", `{"before_message_id": 0}`},
		"negative":          {"s1", `{"before_message_id": -7}`},
		"string":            {"s1", `{"before_message_id": "7"}`},
		"fraction":          {"s1", `{"before_message_id": 7.5}`},
		"missing":           {"s1", `{}`},
		"not json":          {"s1", `7`},
		"empty body":        {"s1", ``},
		"too big to be one": {"s1", `{"before_message_id": 99999999999999999999}`},
	} {
		if st, body := e.rewind(c[0], c[1]); st != 400 || !strings.Contains(body, `"bad_request"`) {
			t.Errorf("%s: %d %s", name, st, body)
		}
	}
	if _, err := os.Stat(argvFile); err == nil {
		t.Fatal("the interpreter ran for a refused request")
	}

	e.h.Hermes = filepath.Join(t.TempDir(), "missing-hermes")
	if st, body := e.rewind("s1", `{"before_message_id": 7}`); st != 502 || !strings.Contains(body, "print-runtime-command") {
		t.Fatalf("no hermes: %d %s", st, body)
	}
}
