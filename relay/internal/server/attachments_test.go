package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/fakehermes"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// testImage encodes a w×h image; seed makes the bytes differ.
func testImage(t *testing.T, format string, w, h int, seed uint8) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.RGBA{seed, 0, 0, 255}})
	img.SetColorIndex(0, 0, 1)
	var b bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&b, img)
	case "jpeg":
		err = jpeg.Encode(&b, img, nil)
	case "gif":
		err = gif.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// raw sends one request and returns the response (body read) for header checks.
func (r *relay) raw(method, path, token string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.http.URL+path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (r *relay) upload(token, ctype string, data []byte) (int, []byte) {
	r.t.Helper()
	resp, b := r.raw("POST", "/api/attachments", token, data, map[string]string{"Content-Type": ctype})
	return resp.StatusCode, b
}

func TestAttachmentUploadDownload(t *testing.T) {
	r := newRelay(t, newFake(t, "plain"))
	viewer, manager := r.pair("pixel-8"), r.pairAs("macbook", store.RoleManager)
	pngData := testImage(t, "png", 1280, 800, 1)

	status, body := r.upload(manager, "image/png", pngData)
	want := fmt.Sprintf(`{"bytes":%d,"height":800,"id":"%s","mime":"image/png","width":1280}`, len(pngData), sha(pngData))
	if status != 201 || string(body) != want {
		t.Fatalf("upload: %d %s, want %s", status, body, want)
	}
	if status, body := r.upload(manager, "image/png", pngData); status != 200 || string(body) != want {
		t.Fatalf("same bytes again: %d %s", status, body)
	}
	for _, f := range []struct{ format, mime string }{{"jpeg", "image/jpeg"}, {"gif", "image/gif"}} {
		data := testImage(t, f.format, 3, 2, 2)
		if status, body := r.upload(manager, f.mime+"; charset=binary", data); status != 201 || !strings.Contains(string(body), `"height":2,`) || !strings.Contains(string(body), `"width":3}`) {
			t.Fatalf("%s: %d %s", f.format, status, body)
		}
	}

	refused := []struct {
		name, token, ctype string
		data               []byte
		status             int
		code               string
	}{
		{"viewer", viewer, "image/png", pngData, 403, "forbidden"},
		{"no token", "", "image/png", pngData, 401, "unauthorized"},
		{"not an image type", manager, "text/plain", pngData, 400, "bad_request"},
		{"webp", manager, "image/webp", pngData, 400, "bad_request"},
		{"no type", manager, "", pngData, 400, "bad_request"},
		{"png bytes as jpeg", manager, "image/jpeg", pngData, 400, "bad_request"},
		{"gif bytes as png", manager, "image/png", testImage(t, "gif", 2, 2, 3), 400, "bad_request"},
		{"garbage", manager, "image/png", []byte("\x89PNG\r\n\x1a\nnot really"), 400, "bad_request"},
		{"empty", manager, "image/gif", nil, 400, "bad_request"},
		{"over 8 MiB", manager, "image/png", append(append([]byte{}, pngData...), make([]byte, 8<<20)...), 413, "too_large"},
	}
	for _, c := range refused {
		if status, body := r.upload(c.token, c.ctype, c.data); status != c.status || errCode(body) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, status, body, c.status, c.code)
		}
	}

	// Download: any paired device; bytes, type and caching headers; ETag revalidation.
	id := sha(pngData)
	resp, got := r.raw("GET", "/api/attachments/"+id, viewer, nil, nil)
	if resp.StatusCode != 200 || !bytes.Equal(got, pngData) || resp.Header.Get("Content-Type") != "image/png" ||
		resp.Header.Get("Cache-Control") != "private, max-age=31536000, immutable" || resp.Header.Get("ETag") != `"`+id+`"` {
		t.Fatalf("download: %d %v (%d bytes)", resp.StatusCode, resp.Header, len(got))
	}
	if resp, _ := r.raw("GET", "/api/attachments/"+id, viewer, nil, map[string]string{"If-None-Match": `"` + id + `"`}); resp.StatusCode != 304 {
		t.Fatalf("revalidation: %d", resp.StatusCode)
	}
	for _, path := range []string{"/api/attachments/" + strings.Repeat("0", 64), "/api/attachments/" + strings.ToUpper(id),
		"/api/attachments/..%2Frelay.db", "/api/attachments/abc"} {
		if resp, body := r.raw("GET", path, viewer, nil, nil); resp.StatusCode != 404 || errCode(body) != "not_found" {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
	if resp, _ := r.raw("GET", "/api/attachments/"+id, "", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("download without a token: %d", resp.StatusCode)
	}
}

// promptWith sends a prompt with attachments and returns the ack or error.
func (cl *client) promptWith(clientMsgID, sessionID, text string, ids ...string) msg {
	cl.t.Helper()
	cl.send(map[string]any{"type": "prompt", "client_msg_id": clientMsgID, "session_id": sessionID, "text": text, "attachments": ids})
	got := cl.until(func(m msg) bool { return m.str("type") == "ack" || m.str("type") == "error" })
	return got[len(got)-1]
}

func TestPromptWithAttachments(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	manager := r.pairAs("macbook", store.RoleManager)
	a := r.connect(r.pair("pixel-8")) // a viewer may prompt with images a manager uploaded
	sid := fake.AddSession("api_server")
	a.subscribe(sid)
	img1, img2 := testImage(t, "png", 4, 3, 10), testImage(t, "gif", 2, 2, 11)
	for _, data := range [][]byte{img1, img2} {
		if status, body := r.upload(manager, http.DetectContentType(data), data); status != 201 {
			t.Fatalf("upload: %d %s", status, body)
		}
	}
	url1 := secret("data:image/png;base64,"+base64.StdEncoding.EncodeToString(img1), "image data URL")
	url2 := secret("data:image/gif;base64,"+base64.StdEncoding.EncodeToString(img2), "image data URL")

	// Images only: no text part, both images in order.
	cmid := "6f1c2a9e-1b7d-4c1e-9a35-a00000000001"
	ack := a.promptWith(cmid, sid, "", sha(img1), sha(img2))
	if ack.str("type") != "ack" || ack["replayed"] != false {
		t.Fatalf("ack %v", ack)
	}
	a.until(isRunEnd)
	inputs := fake.RunInputsSeen()
	wantInput := `[{"content":[{"image_url":{"url":"` + url1 + `"},"type":"image_url"},{"image_url":{"url":"` + url2 + `"},"type":"image_url"}],"role":"user"}]`
	if len(inputs) != 1 || inputs[0] != wantInput {
		t.Fatalf("hermes input %q", inputs)
	}
	// A resend is acknowledged as the same run, without a second Hermes call.
	if m := a.promptWith(cmid, sid, "", sha(img1), sha(img2)); m.str("run_id") != ack.str("run_id") || m["replayed"] != true {
		t.Fatalf("resend %v", m)
	}
	if n := len(fake.RunInputsSeen()); n != 1 {
		t.Fatalf("%d runs after a resend", n)
	}

	// Text and an image.
	text := secret("secret caption for the screenshot", "prompt text")
	if m := a.promptWith("6f1c2a9e-1b7d-4c1e-9a35-a00000000002", sid, text, sha(img2)); m.str("type") != "ack" {
		t.Fatalf("text+image: %v", m)
	}
	a.until(isRunEnd)
	if in := fake.RunInputsSeen()[1]; !strings.HasPrefix(in, `[{"content":[{"text":"`+text+`","type":"text"},{"image_url":{"url":"`+url2) {
		t.Fatalf("text+image input %q", in)
	}

	// Unknown id; more than 7 MiB in one prompt; empty text without images (schema).
	if m := a.promptWith("6f1c2a9e-1b7d-4c1e-9a35-a00000000003", sid, "x", strings.Repeat("ab", 32)); m.str("code") != "bad_request" ||
		m.str("message") != "unknown attachment" || m.str("ref") != "6f1c2a9e-1b7d-4c1e-9a35-a00000000003" {
		t.Fatalf("unknown attachment: %v", m)
	}
	var big []string
	for i := range 2 {
		data := []byte(fmt.Sprintf("pretend 4 MiB image %d", i))
		id := sha(data)
		if _, err := r.st.PutAttachment(context.Background(), store.Attachment{ID: id, Mime: "image/png", Bytes: 4 << 20, Width: 1, Height: 1}, data); err != nil {
			t.Fatal(err)
		}
		big = append(big, id)
	}
	if m := a.promptWith("6f1c2a9e-1b7d-4c1e-9a35-a00000000004", sid, "x", big...); m.str("code") != "too_large" {
		t.Fatalf("over 7 MiB: %v", m)
	}
	a.send(map[string]any{"type": "prompt", "client_msg_id": "6f1c2a9e-1b7d-4c1e-9a35-a00000000005", "session_id": sid, "text": ""})
	if m := a.until(isType("error"))[0]; m.str("code") != "bad_request" {
		t.Fatalf("empty text without images: %v", m)
	}
	if n := len(fake.RunInputsSeen()); n != 2 {
		t.Fatalf("refused prompts reached Hermes: %d runs", n)
	}

	// During a run: busy, as for text.
	hold := make(chan struct{})
	fake.SetScript(sid, fakehermes.Script{Fixture: fixture(t, "plain"), Hold: map[int]chan struct{}{0: hold}, FromStart: true})
	if _, m := a.prompt(sid); m.str("type") != "ack" {
		t.Fatalf("held prompt: %v", m)
	}
	if m := a.promptWith("6f1c2a9e-1b7d-4c1e-9a35-a00000000006", sid, "", sha(img1)); m.str("code") != "busy" {
		t.Fatalf("image prompt during a run: %v", m)
	}
	close(hold)
	a.until(isRunEnd)
}

// History: user messages of image runs get attachments by time, per page, other fields untouched; deleting the
// session drops the images no other session uses.
func TestHistoryAttachmentsAndDelete(t *testing.T) {
	fake := newFake(t, "plain")
	r := newRelay(t, fake)
	manager := r.pairAs("macbook", store.RoleManager)
	a := r.connect(manager)
	sid, other := fake.AddSession("api_server"), fake.AddSession("api_server")
	a.subscribe(sid)
	a.subscribe(other)
	shared, own := testImage(t, "png", 4, 3, 20), testImage(t, "jpeg", 2, 5, 21)
	for _, data := range [][]byte{shared, own} {
		if status, body := r.upload(manager, http.DetectContentType(data), data); status != 201 {
			t.Fatalf("upload: %d %s", status, body)
		}
	}
	var created []float64
	for i, ids := range [][]string{{sha(shared), sha(own)}, {sha(own)}} {
		before := float64(time.Now().UnixMilli()) / 1000
		if m := a.promptWith(fmt.Sprintf("6f1c2a9e-1b7d-4c1e-9a35-b0000000000%d", i), sid, "look", ids...); m.str("type") != "ack" {
			t.Fatalf("prompt: %v", m)
		}
		a.until(isRunEnd)
		created = append(created, before)
	}
	if m := a.promptWith("6f1c2a9e-1b7d-4c1e-9a35-b00000000009", other, "", sha(shared)); m.str("type") != "ack" {
		t.Fatalf("prompt: %v", m)
	}
	a.until(isRunEnd)
	t1, t2 := created[0], created[1]
	user := func(id int, ts any) map[string]any {
		return map[string]any{"id": id, "session_id": sid, "role": "user", "content": "look\n[screenshot]", "timestamp": ts,
			"token_count": nil, "display_kind": nil, "future_field": map[string]any{"nested": []any{1, "<b>"}}}
	}
	fake.SetMessages(sid, []map[string]any{
		user(1, t1-60),  // before: no images
		user(2, t1+0.2), // run 1
		{"id": 3, "role": "assistant", "content": "a cat", "timestamp": t1 + 3},
		user(4, t2+10), // run 2 (stamped late, still within 120 s)
		{"id": 5, "role": "assistant", "content": "a dog", "timestamp": t2 + 12},
		user(6, t2+300), // after: no images
		user(7, nil),
	})
	type message map[string]json.RawMessage
	page := func(q string) []message {
		t.Helper()
		status, body := r.do("GET", "/api/sessions/"+sid+"/messages"+q, manager, nil)
		var out struct {
			Object string    `json:"object"`
			Data   []message `json:"data"`
		}
		if status != 200 || json.Unmarshal(body, &out) != nil || out.Object != "list" {
			t.Fatalf("messages%s: %d %s", q, status, body)
		}
		return out.Data
	}
	atts := func(m message) string { return string(m["attachments"]) }
	both := fmt.Sprintf(`[{"height":3,"id":"%s","mime":"image/png","width":4},{"height":5,"id":"%s","mime":"image/jpeg","width":2}]`, sha(shared), sha(own))
	second := fmt.Sprintf(`[{"height":5,"id":"%s","mime":"image/jpeg","width":2}]`, sha(own))

	all := page("")
	if len(all) != 7 || atts(all[0]) != "" || atts(all[1]) != both || atts(all[2]) != "" || atts(all[3]) != second ||
		atts(all[5]) != "" || atts(all[6]) != "" {
		t.Fatalf("full page: %v", all)
	}
	var future struct{ Nested []any }
	if json.Unmarshal(all[1]["future_field"], &future) != nil || len(future.Nested) != 2 || future.Nested[1] != "<b>" ||
		string(all[1]["content"]) != `"look\n[screenshot]"` || string(all[1]["display_kind"]) != "null" {
		t.Fatalf("fields changed: %v", all[1])
	}
	// Pages are matched on their own; limit and offset reach Hermes.
	if p := page("?limit=2&offset=0"); len(p) != 2 || atts(p[0]) != "" || atts(p[1]) != both {
		t.Fatalf("page 1: %v", p)
	}
	if p := page("?limit=3&offset=1"); len(p) != 3 || atts(p[0]) != both || atts(p[1]) != "" || atts(p[2]) != second {
		t.Fatalf("page 2: %v", p)
	}
	if p := page("?limit=2&offset=5"); len(p) != 2 || atts(p[0]) != "" || atts(p[1]) != "" {
		t.Fatalf("page 3: %v", p)
	}
	// Another session's history isn't touched by these runs.
	fake.SetMessages(other, []map[string]any{user(1, t1+0.2)})
	if _, body := r.do("GET", "/api/sessions/"+other+"/messages", manager, nil); strings.Contains(string(body), sha(own)) {
		t.Fatalf("other session got this session's image: %s", body)
	}

	if status, body := r.do("DELETE", "/api/sessions/"+sid, manager, nil); status != 200 {
		t.Fatalf("delete: %d %s", status, body)
	}
	if resp, _ := r.raw("GET", "/api/attachments/"+sha(own), manager, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("an image only the deleted session used is still served: %d", resp.StatusCode)
	}
	if resp, _ := r.raw("GET", "/api/attachments/"+sha(shared), manager, nil, nil); resp.StatusCode != 200 {
		t.Fatalf("an image another session uses was deleted: %d", resp.StatusCode)
	}
}
