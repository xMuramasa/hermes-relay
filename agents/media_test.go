package main

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

var (
	pngHead  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpegHead = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	gifHead  = []byte("GIF89a\x01\x00\x01\x00")
	webpHead = []byte("RIFF\x10\x00\x00\x00WEBPVP8 ")
)

func (e *env) media(path string) (int, string, []byte) {
	e.t.Helper()
	resp, err := http.Get(e.srv.URL + "/media?path=" + url.QueryEscape(path))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), b
}

func TestMedia(t *testing.T) {
	e := newEnv(t, "")
	home := e.h.Home
	put := func(rel string, data []byte) string {
		p := filepath.Join(home, rel)
		e.write(p, string(data))
		return p
	}
	cached := put(".hermes/cache/images/img_1.jpg", append(jpegHead, "telegram"...))
	shot := put("Pictures/shot.png", append(pngHead, "rest"...))
	upper := put("Pictures/anim.GIF", gifHead)
	webp := put("work/out.webp", webpHead)

	for _, c := range []struct{ path, mime string }{{cached, "image/jpeg"}, {shot, "image/png"}, {upper, "image/gif"}, {webp, "image/webp"}} {
		st, ct, body := e.media(c.path)
		want, _ := os.ReadFile(c.path)
		if st != 200 || ct != c.mime || !bytes.Equal(body, want) {
			t.Errorf("%s: %d %s %q", c.path, st, ct, body)
		}
	}
	resp, err := http.Get(e.srv.URL + "/media?path=" + url.QueryEscape(shot))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "private, max-age=3600" {
		t.Fatalf("cache-control %q", cc)
	}

	outside := filepath.Join(filepath.Dir(home), "outside-home.png")
	if err := os.WriteFile(outside, pngHead, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })
	link := filepath.Join(home, "Pictures", "link.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	dotLink := filepath.Join(home, "Pictures", "key.png")
	put(".ssh/key.png", pngHead)
	if err := os.Symlink(filepath.Join(home, ".ssh", "key.png"), dotLink); err != nil {
		t.Fatal(err)
	}
	inLink := filepath.Join(home, "Pictures", "ok-link.png")
	if err := os.Symlink(shot, inLink); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := e.media(inLink); st != 200 {
		t.Fatalf("a symlink to an allowed image: %d", st)
	}
	big := put("Pictures/big.png", append(pngHead, make([]byte, maxMedia)...))

	refused := map[string]string{
		"traversal":                    filepath.Join(home, "Pictures") + "/../Pictures/shot.png",
		"traversal out of home":        home + "/../outside-home.png",
		"relative":                     "Pictures/shot.png",
		"empty":                        "",
		"outside home":                 outside,
		"symlink out of home":          link,
		"symlink into a dot-dir":       dotLink,
		"dot-dir":                      put(".config/app/icon.png", pngHead),
		"dot-file":                     put("Pictures/.hidden.png", pngHead),
		"other .hermes dir":            put(".hermes/secrets/x.png", pngHead),
		"wrong extension":              put("Pictures/notes.txt", pngHead),
		"no extension":                 put("Pictures/shot", pngHead),
		"magic mismatch":               put("Pictures/fake.png", jpegHead),
		"text as png":                  put(".hermes/cache/images/img_2.png", []byte("not an image at all")),
		"over 10 MiB":                  big,
		"directory":                    filepath.Join(home, "Pictures.png"),
		"missing":                      filepath.Join(home, "Pictures", "missing.png"),
		"double slash":                 home + "//Pictures/shot.png",
		"home itself":                  home,
		".hermes/cache itself as file": filepath.Join(home, ".hermes", "cache"),
	}
	if err := os.MkdirAll(filepath.Join(home, "Pictures.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, p := range refused {
		if st, ct, body := e.media(p); st != 404 || ct != "application/json" || !bytes.Contains(body, []byte(`"not_found"`)) {
			t.Errorf("%s (%s): %d %s %s", name, p, st, ct, body)
		}
	}
}
