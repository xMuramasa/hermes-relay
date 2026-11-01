package server_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// jobAnswer is a REST answer of the jobs endpoints.
type jobAnswer struct {
	Jobs  []map[string]any `json:"jobs"`
	Job   map[string]any   `json:"job"`
	OK    bool             `json:"ok"`
	Runs  []map[string]any `json:"runs"`
	Error struct{ Code, Message string }
}

func (r *relay) jobs(method, path, token string, body any) (int, jobAnswer) {
	r.t.Helper()
	status, b := r.do(method, path, token, body)
	var out jobAnswer
	if err := json.Unmarshal(b, &out); err != nil {
		r.t.Fatalf("%s %s: %d %s", method, path, status, b)
	}
	return status, out
}

func TestJobsLifecycle(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	mac := r.pairAs("mac", store.RoleManager)
	prompt := secret("secret cron prompt: summarize the ocean news", "job prompt")

	status, a := r.jobs("POST", "/api/jobs", mac, map[string]any{"name": "News", "schedule": "0 8 * * *",
		"prompt": prompt, "deliver": "local", "skills": []string{"news"}, "repeat": 3})
	if status != 200 || a.Job["name"] != "News" || a.Job["prompt"] != prompt || a.Job["schedule_display"] != "0 8 * * *" {
		t.Fatalf("create: %d %+v", status, a)
	}
	id := a.Job["id"].(string)
	for _, k := range []string{"origin", "latest_execution", "base_url", "failure_streak"} {
		if _, ok := a.Job[k]; ok {
			t.Fatalf("job carries %s: %v", k, a.Job)
		}
	}
	if status, a = r.jobs("POST", "/api/jobs/"+id+"/pause", mac, nil); status != 200 || a.Job["state"] != "paused" {
		t.Fatalf("pause: %d %+v", status, a)
	}
	// Hermes leaves disabled (paused) jobs out of its list unless asked; the app needs them to resume them.
	if status, a = r.jobs("GET", "/api/jobs", mac, nil); status != 200 || len(a.Jobs) != 1 || a.Jobs[0]["id"] != id {
		t.Fatalf("list: %d %+v", status, a)
	}
	if _, ok := a.Jobs[0]["latest_execution"]; ok {
		t.Fatalf("list job not trimmed: %v", a.Jobs[0])
	}
	if status, a = r.jobs("POST", "/api/jobs/"+id+"/resume", mac, nil); status != 200 || a.Job["enabled"] != true {
		t.Fatalf("resume: %d %+v", status, a)
	}
	if status, a = r.jobs("POST", "/api/jobs/"+id+"/run", mac, map[string]any{"prompt": "ignored"}); status != 200 || a.Job["id"] != id {
		t.Fatalf("run: %d %+v", status, a)
	}
	status, a = r.jobs("PATCH", "/api/jobs/"+id, mac, map[string]any{"name": "Morning news", "schedule": "0 7 * * *",
		"prompt": prompt + " 2", "enabled": true, "repeat": 0, "skill": "news"})
	if status != 200 || a.Job["name"] != "Morning news" || a.Job["schedule_display"] != "0 7 * * *" {
		t.Fatalf("update: %d %+v", status, a)
	}
	if status, a = r.jobs("GET", "/api/jobs/"+id, mac, nil); status != 200 || a.Job["name"] != "Morning news" {
		t.Fatalf("get: %d %+v", status, a)
	}
	if status, a = r.jobs("DELETE", "/api/jobs/"+id, mac, nil); status != 200 || !a.OK {
		t.Fatalf("delete: %d %+v", status, a)
	}
	if status, a = r.jobs("GET", "/api/jobs/"+id, mac, nil); status != 404 || a.Error.Code != "not_found" {
		t.Fatalf("get deleted: %d %+v", status, a)
	}
	if !strings.Contains(suiteLogs.String(), `"msg":"job pause","test":"TestJobsLifecycle","job_id":"`+id+`","device":"mac","outcome":"ok"`) {
		t.Fatal("job pause not logged with its id and outcome")
	}
}

func TestJobsForbiddenForViewers(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	phone := r.pair("pixel-8")
	id := fake.AddJob(map[string]any{"prompt": "p"})

	for _, path := range []string{"/api/jobs", "/api/jobs/" + id, "/api/jobs/" + id + "/runs"} {
		if status, a := r.jobs("GET", path, phone, nil); status != 200 || a.Error.Code != "" {
			t.Fatalf("viewer GET %s: %d %+v", path, status, a)
		}
	}
	body := map[string]any{"name": "x", "schedule": "0 9 * * *", "prompt": "p"}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/jobs", body}, {"PATCH", "/api/jobs/" + id, map[string]any{"name": "y"}},
		{"DELETE", "/api/jobs/" + id, nil}, {"POST", "/api/jobs/" + id + "/pause", nil},
		{"POST", "/api/jobs/" + id + "/resume", nil}, {"POST", "/api/jobs/" + id + "/run", nil},
	} {
		if status, a := r.jobs(c.method, c.path, phone, c.body); status != 403 || a.Error.Code != "forbidden" {
			t.Fatalf("viewer %s %s: %d %+v", c.method, c.path, status, a)
		}
		if status, _ := r.do(c.method, c.path, "", c.body); status != 401 {
			t.Fatalf("no token %s %s: %d", c.method, c.path, status)
		}
	}
	_, _, _, _, paths := fake.Snapshot()
	for _, p := range paths {
		if strings.Contains(p, "/api/jobs") && !strings.HasPrefix(p, "GET ") {
			t.Fatalf("a viewer's write reached Hermes: %s", p)
		}
	}
	if fake.Job(id)["name"] != "job "+id {
		t.Fatal("job changed")
	}
}

func TestJobsAllowlist(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	mac := r.pairAs("mac", store.RoleManager)
	id := fake.AddJob(map[string]any{"prompt": "p", "model": "claude-sonnet-5-5", "provider": "anthropic",
		"script": "fetch.py", "workdir": "/home/hermes/work", "enabled_toolsets": []any{"web"}, "context_from": []any{"aaaaaaaaaaaa"}})

	// Read-only fields are shown as Hermes has them.
	status, a := r.jobs("GET", "/api/jobs/"+id, mac, nil)
	if status != 200 || a.Job["model"] != "claude-sonnet-5-5" || a.Job["provider"] != "anthropic" ||
		a.Job["script"] != "fetch.py" || a.Job["workdir"] != "/home/hermes/work" || a.Job["enabled_toolsets"] == nil ||
		a.Job["context_from"] == nil {
		t.Fatalf("get: %d %+v", status, a)
	}
	// ... but never written.
	base := map[string]any{"name": "x", "schedule": "0 9 * * *", "prompt": "p"}
	for _, k := range []string{"model", "provider", "script", "workdir", "enabled_toolsets", "context_from",
		"failure_deliver", "origin", "no_agent", "base_url", "enabled", "skill", "id"} {
		body := map[string]any{k: "x"}
		for bk, v := range base {
			body[bk] = v
		}
		if status, a := r.jobs("POST", "/api/jobs", mac, body); status != 400 || a.Error.Code != "bad_request" {
			t.Fatalf("create with %s: %d %+v", k, status, a)
		}
	}
	for _, k := range []string{"model", "provider", "script", "workdir", "enabled_toolsets", "context_from",
		"paused", "paused_reason", "state", "next_run_at", "id"} {
		if status, a := r.jobs("PATCH", "/api/jobs/"+id, mac, map[string]any{k: "x"}); status != 400 || a.Error.Code != "bad_request" {
			t.Fatalf("update with %s: %d %+v", k, status, a)
		}
	}
	if n := len(fake.JobBodies()); n != 0 {
		t.Fatalf("refused bodies reached Hermes: %v", fake.JobBodies())
	}
	// Every allowed field gets through, and nothing else.
	status, a = r.jobs("POST", "/api/jobs", mac, map[string]any{"name": "n", "schedule": "every 2h", "prompt": "p",
		"deliver": "telegram", "skills": []string{"s"}, "repeat": 2, "paused": true, "paused_reason": "later"})
	if status != 200 || a.Job["state"] != "paused" || a.Job["paused_reason"] != "later" {
		t.Fatalf("create: %d %+v", status, a)
	}
	status, _ = r.jobs("PATCH", "/api/jobs/"+id, mac, map[string]any{"name": "n", "schedule": "0 9 * * *", "prompt": "p",
		"deliver": "local", "skills": []string{}, "skill": "s", "repeat": 1, "enabled": false})
	if status != 200 {
		t.Fatalf("update: %d", status)
	}
	bodies := fake.JobBodies()
	if len(bodies) != 2 || len(bodies[0]) != 8 || len(bodies[1]) != 8 {
		t.Fatalf("bodies sent: %v", bodies)
	}
	if fake.Job(id)["model"] != "claude-sonnet-5-5" || fake.Job(id)["workdir"] != "/home/hermes/work" {
		t.Fatalf("read-only fields changed: %v", fake.Job(id))
	}

	// Hermes' own refusals come back with its message.
	for _, c := range []struct {
		method, path string
		body         any
		status       int
		code, msg    string
	}{
		{"POST", "/api/jobs", map[string]any{"name": strings.Repeat("n", 201), "schedule": "0 9 * * *", "prompt": "p"},
			400, "bad_request", "Name must be ≤ 200 characters"},
		{"POST", "/api/jobs", map[string]any{"name": "n", "schedule": "0 9 * * *"}, 400, "bad_request", "Cron job has nothing to run"},
		{"POST", "/api/jobs", map[string]any{"name": "n", "schedule": "0 9 * * *", "prompt": "p", "repeat": 0},
			400, "bad_request", "Repeat must be a positive integer"},
		{"PATCH", "/api/jobs/" + id, map[string]any{}, 400, "bad_request", "No valid fields to update"},
		{"PATCH", "/api/jobs/" + id, map[string]any{"schedule": "never"}, 502, "hermes_error", "Invalid schedule 'never'"},
		{"POST", "/api/jobs", map[string]any{"name": 5}, 400, "bad_request", "documented fields"},
	} {
		status, a := r.jobs(c.method, c.path, mac, c.body)
		if status != c.status || a.Error.Code != c.code || !strings.Contains(a.Error.Message, c.msg) {
			t.Fatalf("%s %s %v: %d %+v", c.method, c.path, c.body, status, a)
		}
	}
}

func TestJobsErrors(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	mac := r.pairAs("mac", store.RoleManager)
	for _, path := range []string{"/api/jobs/ABCDEF123456", "/api/jobs/abc", "/api/jobs/0123456789abc/runs", "/api/jobs/x/pause"} {
		method := "GET"
		if strings.HasSuffix(path, "pause") {
			method = "POST"
		}
		if status, a := r.jobs(method, path, mac, nil); status != 404 || a.Error.Code != "not_found" {
			t.Fatalf("%s %s: %d %+v", method, path, status, a)
		}
	}
	if status, a := r.jobs("POST", "/api/jobs/0123456789ab/pause", mac, nil); status != 404 || a.Error.Code != "not_found" {
		t.Fatalf("unknown job: %d %+v", status, a)
	}
	fake.FailJobs(501)
	if status, a := r.jobs("GET", "/api/jobs", mac, nil); status != 503 || a.Error.Code != "hermes_unavailable" {
		t.Fatalf("no cron: %d %+v", status, a)
	}
	fake.FailJobs(424)
	if status, a := r.jobs("POST", "/api/jobs", mac, map[string]any{"name": "n", "schedule": "s", "prompt": "p"}); status != 502 ||
		!strings.Contains(a.Error.Message, "the job was saved") {
		t.Fatalf("scheduler registration: %d %+v", status, a)
	}
	fake.FailJobs(0)
	fake.Close()
	if status, a := r.jobs("GET", "/api/jobs", mac, nil); status != 503 || a.Error.Code != "hermes_unavailable" {
		t.Fatalf("Hermes down: %d %+v", status, a)
	}
}

func TestJobRuns(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	phone := r.pair("pixel-8")
	job, other := fake.AddJob(nil), fake.AddJob(nil)
	fake.AddSession("api_server")
	// 150 sessions of another job first, so this job's 55 runs straddle Hermes' 200-session pages.
	for i := range 150 {
		fake.AddCronRun(other, fmt.Sprintf("20261001_%06d", i))
	}
	var want []string
	for i := range 55 {
		want = append(want, fake.AddCronRun(job, fmt.Sprintf("20261%03d_090000", 101+i)))
	}
	slices.Reverse(want)

	status, a := r.jobs("GET", "/api/jobs/"+job+"/runs", phone, nil)
	if status != 200 || len(a.Runs) != 50 {
		t.Fatalf("runs: %d %d %+v", status, len(a.Runs), a.Error)
	}
	for i, run := range a.Runs {
		if run["id"] != want[i] || run["title"] == nil || run["started_at"] == nil {
			t.Fatalf("run %d = %v, want %s", i, run, want[i])
		}
	}
	if status, a := r.jobs("GET", "/api/jobs/aaaaaaaaaaaa/runs", phone, nil); status != 200 || a.Runs == nil || len(a.Runs) != 0 {
		t.Fatalf("no runs: %d %+v", status, a)
	}
}
