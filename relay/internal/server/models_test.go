package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

func countPath(paths []string, p string) (n int) {
	for _, x := range paths {
		if x == p {
			n++
		}
	}
	return n
}

type modelEntry struct {
	ID        string
	Reasoning bool
}

func TestModels(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	viewer, manager := r.pair("pixel-8"), r.pairAs("macbook", store.RoleManager)
	api, tg := fake.AddSession("api_server"), fake.AddSession("telegram")

	var list struct {
		Current   store.SessionModel
		Providers []struct {
			Slug, Name string
			Models     []modelEntry
		}
	}
	for _, tok := range []string{viewer, manager} {
		status, body := r.do("GET", "/api/models", tok, nil)
		if status != 200 || json.Unmarshal(body, &list) != nil {
			t.Fatalf("GET /api/models: %d %s", status, body)
		}
		if strings.Contains(string(body), "openrouter") || strings.Contains(string(body), "total_models") {
			t.Fatalf("unconfigured provider or Hermes-only fields: %s", body)
		}
	}
	opus := store.SessionModel{Provider: "claude-subscription-directsdk-experimental", Model: "claude-opus-5-5[1m]"}
	if list.Current != opus || len(list.Providers) != 3 || list.Providers[0].Name != "Claude subscription" ||
		list.Providers[0].Models[0] != (modelEntry{opus.Model, true}) ||
		list.Providers[1].Models[0] != (modelEntry{"claude-haiku-5-5", false}) || list.Providers[2].Slug != "copilot" {
		t.Fatalf("list: %+v", list)
	}

	sessionModels := func() map[string]store.SessionModel {
		t.Helper()
		var out struct{ Sessions map[string]store.SessionModel }
		if status, b := r.do("GET", "/api/session-models", viewer, nil); status != 200 || json.Unmarshal(b, &out) != nil || out.Sessions == nil {
			t.Fatalf("GET /api/session-models: %d %s", status, b)
		}
		return out.Sessions
	}
	if got := sessionModels(); len(got) != 0 {
		t.Fatal(got)
	}
	for i, m := range []store.SessionModel{{Provider: "copilot", Model: "gpt-5.5"}, opus} {
		status, body := r.do("PUT", "/api/sessions/"+api+"/model", []string{manager, viewer}[i], m)
		var got store.SessionModel
		if status != 200 || json.Unmarshal(body, &got) != nil || got != m {
			t.Fatalf("PUT model %v: %d %s", m, status, body)
		}
	}
	if got := sessionModels(); len(got) != 1 || got[api] != opus {
		t.Fatal(got)
	}
	if got := fake.ModelLock(api); got != [2]string{opus.Provider, opus.Model} {
		t.Fatalf("Hermes lock: %v", got)
	}

	for _, bad := range []any{
		map[string]any{"provider": "anthropic", "model": opus.Model},    // another provider's model
		map[string]any{"provider": "openrouter", "model": "some/model"}, // not connected
		map[string]any{"provider": "anthropic"},
		map[string]any{"model": "claude-haiku-5-5"},
		map[string]any{"provider": "anthropic", "model": "claude-haiku-5-5", "model_options": map[string]any{}},
		"[]",
	} {
		if status, b := r.do("PUT", "/api/sessions/"+api+"/model", viewer, bad); status != 400 || errCode(b) != "bad_request" {
			t.Fatalf("PUT %v: %d %s", bad, status, b)
		}
	}
	haiku := store.SessionModel{Provider: "anthropic", Model: "claude-haiku-5-5"}
	if status, b := r.do("PUT", "/api/sessions/"+tg+"/model", viewer, haiku); status != 403 || errCode(b) != "read_only" {
		t.Fatalf("telegram: %d %s", status, b)
	}
	if status, _ := r.do("PUT", "/api/sessions/api_404_nope/model", viewer, haiku); status != 404 {
		t.Fatalf("unknown session: %d", status)
	}
	if fake.ModelLock(tg) != [2]string{} || fake.ModelLock(api)[1] != opus.Model {
		t.Fatal("a refused PUT reached Hermes")
	}
	// Cached: one Hermes call for every GET and PUT above.
	if _, _, _, _, paths := fake.Snapshot(); countPath(paths, "GET /api/model/options") != 1 {
		t.Fatalf("model options fetched %d times", countPath(paths, "GET /api/model/options"))
	}

	// Deleting the session drops its choice.
	if status, _ := r.do("DELETE", "/api/sessions/"+api, viewer, nil); status != 200 {
		t.Fatal(status)
	}
	if got := sessionModels(); len(got) != 0 {
		t.Fatal(got)
	}
}

// Hermes errors map as usual and aren't cached.
func TestModelsHermesDown(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	token := r.pair("pixel-8")
	api := fake.AddSession("api_server")
	fake.FailModelOptions(503)
	if status, b := r.do("GET", "/api/models", token, nil); status != 503 || errCode(b) != "hermes_unavailable" {
		t.Fatalf("GET: %d %s", status, b)
	}
	if status, b := r.do("PUT", "/api/sessions/"+api+"/model", token, map[string]string{"provider": "copilot", "model": "gpt-5.5"}); status != 503 {
		t.Fatalf("PUT: %d %s", status, b)
	}
	fake.FailModelOptions(0)
	if status, b := r.do("GET", "/api/models", token, nil); status != 200 {
		t.Fatalf("after recovery: %d %s", status, b)
	}
}

// The fake refuses what Hermes' _handle_session_model_lock refuses (see the spike notes in the #24 report).
func TestFakeModelLockMatchesHermes(t *testing.T) {
	fake := newFake(t, "plain")
	for _, c := range []struct {
		body   string
		status int
		lock   [2]string
	}{
		{`{"provider": "anthropic", "model": "claude-haiku-5-5"}`, 200, [2]string{"anthropic", "claude-haiku-5-5"}},
		{`{"provider_id": "copilot", "model_id": " gpt-5.5 "}`, 200, [2]string{"copilot", "gpt-5.5"}},
		{`{"model": "anthropic::claude-opus-5-5[1m]"}`, 200, [2]string{"anthropic", "claude-opus-5-5[1m]"}},
		{`{}`, 400, [2]string{}},
		{`{"model": "a\nb"}`, 400, [2]string{}},
		{`[1]`, 400, [2]string{}},
		{`{"provider": "anthropic"}`, 409, [2]string{}},
		{`{"provider": "anthropic", "model": "hermes-agent"}`, 409, [2]string{}},
		{`{"provider": "anthropic", "model": "x"}`, 404, [2]string{}},
	} {
		id := "api_404_nope"
		if c.status != 404 {
			id = fake.AddSession("api_server")
		}
		req, _ := http.NewRequest("POST", fake.URL+"/api/sessions/"+id+"/model", bytes.NewReader([]byte(c.body)))
		req.Header.Set("Authorization", "Bearer "+fakeKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.status || fake.ModelLock(id) != c.lock {
			t.Fatalf("%s: %d %v", c.body, resp.StatusCode, fake.ModelLock(id))
		}
	}
}

// A model chosen for a conversation rides in each run's body: Hermes' /v1/runs ignores the session lock.
func TestChosenModelReachesRuns(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	token := r.pairAs("macbook", store.RoleManager)
	chosen, plain := fake.AddSession("api_server"), fake.AddSession("api_server")
	m := store.SessionModel{Provider: "copilot", Model: "gpt-5.5"}
	if status, b := r.do("PUT", "/api/sessions/"+chosen+"/model", token, m); status != 200 {
		t.Fatalf("PUT model: %d %s", status, b)
	}
	cl := r.connect(token)
	for _, sid := range []string{chosen, plain} {
		if _, ack := cl.prompt(sid); ack.str("type") != "ack" {
			t.Fatalf("prompt in %s: %v", sid, ack)
		}
	}
	if got := fake.RunModelsSeen(); len(got) != 2 || got[0] != "copilot/gpt-5.5" || got[1] != "" {
		t.Fatalf("models sent with the runs: %q", got)
	}
}

// A conversation's reasoning effort is the relay's own record, sent to Hermes as model_options with each run.
func TestSessionEffort(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	viewer, manager := r.pair("pixel-8"), r.pairAs("macbook", store.RoleManager)
	high, off, plain, tg := fake.AddSession("api_server"), fake.AddSession("api_server"), fake.AddSession("api_server"), fake.AddSession("telegram")

	sessionEfforts := func() map[string]string {
		t.Helper()
		var out struct{ Sessions map[string]string }
		if status, b := r.do("GET", "/api/session-efforts", viewer, nil); status != 200 || json.Unmarshal(b, &out) != nil || out.Sessions == nil {
			t.Fatalf("GET /api/session-efforts: %d %s", status, b)
		}
		return out.Sessions
	}
	if got := sessionEfforts(); len(got) != 0 {
		t.Fatal(got)
	}
	for _, c := range []struct {
		id, tok, body string
		status        int
		answer        string
	}{
		{high, manager, `{"effort": "low"}`, 200, `{"effort":"low"}`},
		{high, viewer, `{"effort": "high"}`, 200, `{"effort":"high"}`},
		{off, manager, `{"effort": "none"}`, 200, `{"effort":"none"}`},
		{plain, manager, `{"effort": "max"}`, 200, `{"effort":"max"}`},
		{plain, manager, `{"effort": null}`, 200, `{"effort":null}`},
		{plain, manager, `{"effort": "ultra"}`, 400, ""},
		{plain, manager, `{"effort": "minimal"}`, 400, ""},
		{plain, manager, `{"effort": "HIGH"}`, 400, ""},
		{plain, manager, `{"effort": 3}`, 400, ""},
		{plain, manager, `{}`, 400, ""},
		{plain, manager, `{"effort": "low", "fast": true}`, 400, ""},
		{"api_404_nope", manager, `{"effort": "low"}`, 404, ""},
		{tg, manager, `{"effort": "low"}`, 403, ""},
	} {
		status, b := r.do("PUT", "/api/sessions/"+c.id+"/effort", c.tok, c.body)
		if status != c.status || c.answer != "" && string(bytes.TrimSpace(b)) != c.answer {
			t.Fatalf("PUT %s: %d %s", c.body, status, b)
		}
	}
	if got := sessionEfforts(); len(got) != 2 || got[high] != "high" || got[off] != "none" {
		t.Fatal(got)
	}

	cl := r.connect(manager)
	for _, sid := range []string{high, off, plain} {
		if _, ack := cl.prompt(sid); ack.str("type") != "ack" {
			t.Fatalf("prompt in %s: %v", sid, ack)
		}
	}
	want := []string{`{"reasoning":{"effort":"high"}}`, `{"reasoning":{"enabled":false}}`, ""}
	if got := fake.RunOptionsSeen(); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("model_options sent with the runs: %q", got)
	}

	// The fake's run in that session may still be ending (409 busy while a run writes); slow under -race.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		status, b := r.do("DELETE", "/api/sessions/"+high, manager, nil)
		if status == http.StatusConflict && time.Now().Before(deadline) {
			continue
		}
		if status/100 != 2 {
			t.Fatalf("DELETE: %d %s", status, b)
		}
		break
	}
	if got := sessionEfforts(); len(got) != 1 || got[off] != "none" {
		t.Fatal(got)
	}
}
