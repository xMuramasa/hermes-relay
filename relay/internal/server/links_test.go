package server_test

import (
	"encoding/json"
	"testing"

	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

func TestLinks(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	viewer, manager := r.pair("pixel-8"), r.pairAs("macbook", store.RoleManager)
	a, b, c, tg := fake.AddSession("api_server"), fake.AddSession("api_server"), fake.AddSession("api_server"),
		fake.AddSession("telegram")
	put := func(sid, tok string, body any) (int, []byte) {
		t.Helper()
		return r.do("PUT", "/api/sessions/"+sid+"/continues", tok, body)
	}
	links := func() map[string]string {
		t.Helper()
		var out struct{ Continues map[string]string }
		if status, b := r.do("GET", "/api/links", viewer, nil); status != 200 || json.Unmarshal(b, &out) != nil || out.Continues == nil {
			t.Fatalf("GET /api/links: %d %s", status, b)
		}
		return out.Continues
	}
	if got := links(); len(got) != 0 {
		t.Fatal(got)
	}

	// tg > a > b > c, by both roles, any source; the same link twice is fine.
	for i, l := range [][2]string{{a, tg}, {b, a}, {c, b}, {c, b}} {
		status, body := put(l[0], []string{viewer, manager}[i%2], map[string]any{"from": l[1]})
		if status != 200 || string(body) != `{"from":"`+l[1]+`"}` {
			t.Fatalf("link %v: %d %s", l, status, body)
		}
	}
	if got := links(); len(got) != 3 || got[a] != tg || got[b] != a || got[c] != b {
		t.Fatal(got)
	}

	d := fake.AddSession("api_server")
	for _, bad := range []struct {
		sid  string
		body any
	}{
		{d, map[string]any{"from": a}},     // a is continued by b already
		{tg, map[string]any{"from": c}},    // loop
		{d, map[string]any{"from": d}},     // itself
		{d, map[string]any{"from": "x y"}}, // not an id
		{d, map[string]any{"from": 1}},
		{d, map[string]any{}},
		{d, map[string]any{"from": a, "digest": "..."}},
	} {
		if status, body := put(bad.sid, viewer, bad.body); status != 400 || errCode(body) != "bad_request" {
			t.Fatalf("PUT %v: %d %s", bad.body, status, body)
		}
	}
	for _, sid := range [][2]string{{d, "api_404_nope"}, {"api_404_nope", c}} {
		if status, _ := put(sid[0], viewer, map[string]any{"from": sid[1]}); status != 404 {
			t.Fatalf("unknown session %v: %d", sid, status)
		}
	}

	// Deleting b leaves tg > a and c, unlinked; null removes a link.
	if status, _ := r.do("DELETE", "/api/sessions/"+b, viewer, nil); status != 200 {
		t.Fatal(status)
	}
	if got := links(); len(got) != 1 || got[a] != tg {
		t.Fatal(got)
	}
	if status, body := put(a, manager, map[string]any{"from": nil}); status != 200 || string(body) != `{"from":null}` {
		t.Fatalf("remove: %d %s", status, body)
	}
	if got := links(); len(got) != 0 {
		t.Fatal(got)
	}
}

// A chain holds at most 32 links.
func TestLinkChainLimit(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	token := r.pair("pixel-8")
	prev := fake.AddSession("api_server")
	for i := range 33 {
		next := fake.AddSession("api_server")
		status, body := r.do("PUT", "/api/sessions/"+next+"/continues", token, map[string]any{"from": prev})
		if want := map[bool]int{true: 200, false: 400}[i < 32]; status != want {
			t.Fatalf("link %d: %d %s", i+1, status, body)
		}
		prev = next
	}
}
