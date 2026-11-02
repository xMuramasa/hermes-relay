package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxMedia caps the images /media serves.
const maxMedia = 10 << 20

// mediaTypes are the extensions /media serves and the type their bytes must sniff as.
var mediaTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp"}

// openMedia opens an image the relay may show: a regular file (after symlinks) under the home directory, no path
// component starting with "." except under ~/.hermes/cache/, an image extension its first bytes agree with, at most
// maxMedia. The rules hold for the path as asked and as resolved. ok is false for anything else.
func (h *Helper) openMedia(p string) (f *os.File, mime string, ok bool) {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return nil, "", false
	}
	home, err := filepath.EvalSymlinks(h.Home)
	if err != nil {
		return nil, "", false
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil || !mediaAllowed(filepath.Clean(h.Home), p) || !mediaAllowed(home, real) {
		return nil, "", false
	}
	mime, ok = mediaTypes[strings.ToLower(filepath.Ext(real))]
	if fi, err := os.Lstat(real); !ok || err != nil || !fi.Mode().IsRegular() { // before opening: a FIFO would block
		return nil, "", false
	}
	f, err = os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", false
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxMedia || http.DetectContentType(head[:n]) != mime {
		f.Close()
		return nil, "", false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, "", false
	}
	return f, mime, true
}

// mediaAllowed applies the location rules to a clean absolute path.
func mediaAllowed(home, p string) bool {
	rel, err := filepath.Rel(home, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	if strings.HasPrefix(rel, ".hermes/cache/") {
		return true
	}
	for _, c := range strings.Split(rel, "/") {
		if strings.HasPrefix(c, ".") {
			return false
		}
	}
	return true
}

// serveMedia answers GET /media?path=<absolute path>.
func (h *Helper) serveMedia(w http.ResponseWriter, r *http.Request) {
	f, mime, ok := h.openMedia(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such image")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = io.Copy(w, io.LimitReader(f, maxMedia))
}
