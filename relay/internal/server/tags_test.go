package server_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

func errCode(body []byte) string {
	var out struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Error.Code
}

// Pin and rename work on every source; delete only on api_server, cron and cli. Both roles may do both.
func TestSessionPinRenameDelete(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	viewer, manager := r.pair("pixel-8"), r.pairAs("macbook", store.RoleManager)
	tg, cron, cli, api, discord := fake.AddSession("telegram"), fake.AddSession("cron"), fake.AddSession("cli"),
		fake.AddSession("api_server"), fake.AddSession("discord")
	title := secret("Title-not-for-logs", "session title")

	for _, sid := range []string{tg, cron} {
		for _, tok := range []string{viewer, manager} {
			if status, body := r.do("PATCH", "/api/sessions/"+sid, tok, map[string]any{"title": "  " + title + "  ", "pinned": true}); status != 200 {
				t.Fatalf("pin+rename %s: %d %s", sid, status, body)
			}
		}
		var got struct {
			Session struct {
				Title  string
				Pinned bool
			} `json:"session"`
		}
		_, body := r.do("GET", "/api/sessions/"+sid, viewer, nil)
		if json.Unmarshal(body, &got) != nil || got.Session.Title != title || !got.Session.Pinned {
			t.Fatalf("after patch %s: %s", sid, body)
		}
		if status, _ := r.do("PATCH", "/api/sessions/"+sid, viewer, map[string]any{"pinned": false}); status != 200 {
			t.Fatalf("unpin %s: %d", sid, status)
		}
		_, body = r.do("GET", "/api/sessions/"+sid, viewer, nil)
		if json.Unmarshal(body, &got) != nil || got.Session.Title != title || got.Session.Pinned {
			t.Fatalf("pin only must keep the title %s: %s", sid, body)
		}
	}
	for _, bad := range []any{map[string]any{}, map[string]any{"archived": true}, map[string]any{"title": " "},
		map[string]any{"title": strings.Repeat("x", 201)}, map[string]any{"pinned": "yes"}, map[string]any{"pinned": nil}} {
		if status, body := r.do("PATCH", "/api/sessions/"+cron, viewer, bad); status != 400 || errCode(body) != "bad_request" {
			t.Fatalf("PATCH %v: %d %s", bad, status, body)
		}
	}
	if status, _ := r.do("PATCH", "/api/sessions/api_404_nope", viewer, map[string]any{"pinned": true}); status != 404 {
		t.Fatalf("unknown session: %d", status)
	}

	for _, sid := range []string{tg, discord} {
		if status, body := r.do("DELETE", "/api/sessions/"+sid, manager, nil); status != 403 || errCode(body) != "read_only" {
			t.Fatalf("delete %s: %d %s", sid, status, body)
		}
	}
	for i, sid := range []string{api, cron, cli} {
		tok := []string{viewer, manager}[i%2]
		if status, body := r.do("DELETE", "/api/sessions/"+sid, tok, nil); status != 200 {
			t.Fatalf("delete %s: %d %s", sid, status, body)
		}
		if status, _ := r.do("GET", "/api/sessions/"+sid, viewer, nil); status != 404 {
			t.Fatalf("%s still there: %d", sid, status)
		}
	}
	if status, _ := r.do("GET", "/api/sessions/"+tg, viewer, nil); status != 200 {
		t.Fatal("telegram session vanished")
	}
}

// Hermes' 409 session_active_turn reaches the app as busy with a clear message; the session and its tags stay.
func TestDeleteWhileTurnActive(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	token := r.pair("pixel-8")
	cron := fake.AddSession("cron")
	if status, _ := r.do("PUT", "/api/sessions/"+cron+"/tags", token, map[string]any{"tags": []string{"news"}}); status != 200 {
		t.Fatal(status)
	}
	fake.SetActiveTurn(cron, true)
	status, body := r.do("DELETE", "/api/sessions/"+cron, token, nil)
	if status != 409 || errCode(body) != "busy" || !strings.Contains(string(body), "a run is still writing") {
		t.Fatalf("delete during a turn: %d %s", status, body)
	}
	if _, body := r.do("GET", "/api/tags", token, nil); !strings.Contains(string(body), cron) {
		t.Fatalf("a refused delete dropped the tags: %s", body)
	}
	fake.SetActiveTurn(cron, false)
	if status, body := r.do("DELETE", "/api/sessions/"+cron, token, nil); status != 200 {
		t.Fatalf("delete after the turn: %d %s", status, body)
	}
}

func TestTags(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	viewer, manager := r.pair("pixel-8"), r.pairAs("macbook", store.RoleManager)
	tg, api := fake.AddSession("telegram"), fake.AddSession("api_server")
	put := func(sid, tok string, body any) (int, []string, []byte) {
		t.Helper()
		status, b := r.do("PUT", "/api/sessions/"+sid+"/tags", tok, body)
		var out struct{ Tags []string }
		_ = json.Unmarshal(b, &out)
		return status, out.Tags, b
	}
	list := func() map[string][]string {
		t.Helper()
		status, b := r.do("GET", "/api/tags", viewer, nil)
		var out struct{ Sessions map[string][]string }
		if status != 200 || json.Unmarshal(b, &out) != nil || out.Sessions == nil {
			t.Fatalf("GET /api/tags: %d %s", status, b)
		}
		return out.Sessions
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("no tags yet: %v", got)
	}

	tag := secret("Tag-not-for-logs", "tag")
	status, tags, b := put(tg, viewer, map[string]any{"tags": []string{" " + tag + " ", "work", "WORK", "tag-not-for-LOGS", "café"}})
	if status != 200 || strings.Join(tags, ",") != tag+",work,café" {
		t.Fatalf("set: %d %s", status, b)
	}
	if status, tags, b := put(api, manager, map[string]any{"tags": []string{"Ideas"}}); status != 200 || len(tags) != 1 {
		t.Fatalf("set as manager: %d %s", status, b)
	}
	if got := list(); strings.Join(got[tg], ",") != tag+",work,café" || strings.Join(got[api], ",") != "Ideas" {
		t.Fatalf("list: %v", got)
	}
	// Replace, not merge.
	if status, _, b := put(tg, viewer, map[string]any{"tags": []string{"news"}}); status != 200 {
		t.Fatalf("replace: %d %s", status, b)
	}
	if got := list(); strings.Join(got[tg], ",") != "news" {
		t.Fatalf("after replace: %v", got)
	}

	ten := make([]string, 10)
	for i := range ten {
		ten[i] = string(rune('a' + i))
	}
	if status, tags, b := put(tg, viewer, map[string]any{"tags": append(ten, "A", "b")}); status != 200 || len(tags) != 10 {
		t.Fatalf("ten after dedupe: %d %s", status, b)
	}
	for _, bad := range []any{
		map[string]any{"tags": append(ten, "k")},
		map[string]any{"tags": []string{"  "}},
		map[string]any{"tags": []string{strings.Repeat("x", 33)}},
		map[string]any{"tags": []string{"tab\there"}},
		map[string]any{"tags": []string{"bell\u0007"}},
		map[string]any{"tags": []any{1}},
		map[string]any{},
		map[string]any{"tags": []string{"ok"}, "extra": 1},
	} {
		if status, _, b := put(tg, viewer, bad); status != 400 || errCode(b) != "bad_request" {
			t.Fatalf("PUT %v: %d %s", bad, status, b)
		}
	}
	if got := list(); len(got[tg]) != 10 {
		t.Fatalf("a refused PUT changed the tags: %v", got)
	}
	if status, tags, _ := put(tg, viewer, map[string]any{"tags": []string{strings.Repeat("é", 32)}}); status != 200 || len(tags) != 1 {
		t.Fatalf("32 characters: %d", status)
	}
	if status, _, _ := put("api_404_nope", viewer, map[string]any{"tags": []string{"x"}}); status != 404 {
		t.Fatalf("unknown session: %d", status)
	}
	if status, _ := r.do("GET", "/api/tags", "", nil); status != 401 {
		t.Fatalf("no token: %d", status)
	}

	// [] clears; a delete drops the session's tags.
	if status, tags, _ := put(tg, viewer, map[string]any{"tags": []string{}}); status != 200 || tags == nil || len(tags) != 0 {
		t.Fatalf("clear: %d %v", status, tags)
	}
	if status, _ := r.do("DELETE", "/api/sessions/"+api, viewer, nil); status != 200 {
		t.Fatal(status)
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("tags left after clear and delete: %v", got)
	}
}
