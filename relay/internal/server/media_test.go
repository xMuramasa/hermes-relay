package server_test

import (
	"bytes"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xMuramasa/trismegistus/relay/internal/agents"
)

func TestSessionMedia(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	viewer := r.pair("pixel-8")
	sid, other := fake.AddSession("telegram"), fake.AddSession("api_server")
	const (
		answerImg = "/home/muramasa/.hermes/cache/screenshots/shot_1.png"
		tgImg     = "/home/muramasa/.hermes/cache/images/img_7f3a.jpg"
		quoted    = "/home/muramasa/Pictures/chart.png"
		notNamed  = "/home/muramasa/.hermes/cache/images/img_other.jpg"
	)
	fake.SetMessages(sid, []map[string]any{
		{"id": 1, "role": "user", "content": "what's this?\n[Image attached at: " + tgImg + "]", "timestamp": 1.0},
		{"id": 2, "role": "assistant", "content": "Here it is:\nMEDIA:" + answerImg + "\nand `MEDIA:" + quoted + "`.", "timestamp": 2.0},
		// Wrong roles: a user can't make the relay serve a path by typing MEDIA:, nor an answer by quoting the note.
		{"id": 3, "role": "user", "content": "MEDIA:" + notNamed, "timestamp": 3.0},
		{"id": 4, "role": "assistant", "content": "[Image attached at: " + notNamed + "]", "timestamp": 4.0},
		{"id": 5, "role": "tool", "content": "MEDIA:" + notNamed, "timestamp": 5.0},
	})
	get := func(session, path, token string) (int, string, []byte) {
		t.Helper()
		resp, body := r.raw("GET", "/api/sessions/"+session+"/media?path="+url.QueryEscape(path), token, nil, nil)
		return resp.StatusCode, resp.Header.Get("Content-Type") + "|" + resp.Header.Get("Cache-Control"), body
	}

	// Helper not configured, then not running: 503 hermes_unavailable, but only for named paths.
	if st, _, body := get(sid, answerImg, viewer); st != 503 || errCode(body) != "hermes_unavailable" {
		t.Fatalf("no helper: %d %s", st, body)
	}
	if st, _, body := get(sid, notNamed, viewer); st != 404 || errCode(body) != "not_found" {
		t.Fatalf("no helper, unnamed path: %d %s", st, body)
	}
	r.srv.SetAgents(agents.New(filepath.Join(t.TempDir(), "missing.sock")))
	if st, _, body := get(sid, tgImg, viewer); st != 503 || errCode(body) != "hermes_unavailable" {
		t.Fatalf("helper down: %d %s", st, body)
	}

	helper := newFakeAgents(t)
	png := []byte("\x89PNG\r\n\x1a\nfake")
	helper.setMedia(map[string][]byte{answerImg: png, tgImg: png, notNamed: png}) // quoted: the helper says 404
	r.srv.SetAgents(agents.New(helper.sock))
	for _, p := range []string{answerImg, tgImg} {
		if st, hdr, body := get(sid, p, viewer); st != 200 || hdr != "image/png|private, max-age=3600" || !bytes.Equal(body, png) {
			t.Fatalf("%s: %d %s %q", p, st, hdr, body)
		}
	}
	if st, _, body := get(sid, quoted, viewer); st != 404 || errCode(body) != "not_found" {
		t.Fatalf("helper 404: %d %s", st, body)
	}
	for name, c := range map[string][2]string{
		"not named":           {sid, notNamed},
		"other session":       {other, answerImg},
		"relative":            {sid, "screenshots/shot_1.png"},
		"empty":               {sid, ""},
		"suffix of a path":    {sid, answerImg + ".png"},
		"prefix of a path":    {sid, strings.TrimSuffix(answerImg, ".png")},
		"unknown session":     {"20261023_999999_ffffff", answerImg},
		"bad session id":      {"..", answerImg},
		"trailing backtick":   {sid, quoted + "`"},
		"trailing full stop.": {sid, quoted + "`."},
	} {
		if st, _, body := get(c[0], c[1], viewer); st != 404 || errCode(body) != "not_found" {
			t.Errorf("%s: %d %s", name, st, body)
		}
	}
	if st, _, _ := get(sid, answerImg, ""); st != 401 {
		t.Fatalf("no token: %d", st)
	}
	if reqs := helper.requests(); strings.Contains(reqs, url.QueryEscape(notNamed)) {
		t.Fatalf("the helper was asked for an unnamed path: %s", reqs)
	}

	// Cached for 60 s: a path added to the history now isn't served yet, and Hermes isn't asked again.
	count := func() (n int) {
		_, _, _, _, paths := fake.Snapshot()
		for _, p := range paths {
			if p == "GET /api/sessions/"+sid+"/messages" {
				n++
			}
		}
		return n
	}
	before := count()
	fake.SetMessages(sid, []map[string]any{{"id": 9, "role": "assistant", "content": "MEDIA:" + notNamed}})
	if st, _, _ := get(sid, notNamed, viewer); st != 404 {
		t.Fatalf("cache: %d", st)
	}
	if st, _, _ := get(sid, answerImg, viewer); st != 200 {
		t.Fatalf("cache: %d", st)
	}
	if n := count(); n != before {
		t.Fatalf("Hermes asked %d more times within the cache time", n-before)
	}
}

// Long histories are read page by page.
func TestSessionMediaPages(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	viewer := r.pair("pixel-8")
	helper := newFakeAgents(t)
	r.srv.SetAgents(agents.New(helper.sock))
	sid := fake.AddSession("telegram")
	const late = "/home/muramasa/.hermes/cache/images/img_late.jpg"
	var msgs []map[string]any
	for i := range 1200 {
		msgs = append(msgs, map[string]any{"id": i, "role": "user", "content": "hello"})
	}
	msgs = append(msgs, map[string]any{"id": 1200, "role": "user", "content": "[Image attached at: " + late + "]"})
	fake.SetMessages(sid, msgs)
	helper.setMedia(map[string][]byte{late: []byte("GIF89a")})
	resp, body := r.raw("GET", "/api/sessions/"+sid+"/media?path="+url.QueryEscape(late), viewer, nil, nil)
	if resp.StatusCode != 200 || string(body) != "GIF89a" {
		t.Fatalf("path on the third page: %d %s", resp.StatusCode, body)
	}
}
