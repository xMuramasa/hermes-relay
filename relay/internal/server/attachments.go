package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/gif" // registers the decoders image.DecodeConfig uses
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

const maxUploadBytes = 8 << 20

// uploadTypes maps the accepted Content-Types to image.DecodeConfig's format names.
var uploadTypes = map[string]string{"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif"}

// uploadAttachment stores an image for a later prompt: 201 when new, 200 when the same bytes were already there.
func (s *Server) uploadAttachment(w http.ResponseWriter, r *http.Request, dev store.Device) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	format, ok := uploadTypes[mt]
	if err != nil || !ok {
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "Content-Type must be image/png, image/jpeg or image/gif"), 0)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUploadBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, protocol.Errorf(protocol.CodeTooLarge, "images are at most 8 MiB"), 0)
		} else {
			writeError(w, protocol.Errorf(protocol.CodeBadRequest, "could not read the image"), 0)
		}
		return
	}
	cfg, got, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || got != format || cfg.Width <= 0 || cfg.Height <= 0 {
		writeError(w, protocol.Errorf(protocol.CodeBadRequest, "the body is not a valid %s image", mt), 0)
		return
	}
	sum := sha256.Sum256(data)
	a := store.Attachment{ID: hex.EncodeToString(sum[:]), Mime: mt, Bytes: int64(len(data)), Width: cfg.Width, Height: cfg.Height}
	created, err := s.store.PutAttachment(r.Context(), a, data)
	if err != nil {
		s.log.Error("storing attachment failed", "err", err.Error())
		writeError(w, protocol.Errorf(protocol.CodeInternal, "internal error"), 0)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.log.Info("attachment uploaded", "attachment_id", a.ID, "bytes", a.Bytes, "device", dev.Name, "new", created)
	writeJSON(w, status, map[string]any{"id": a.ID, "mime": a.Mime, "width": a.Width, "height": a.Height, "bytes": a.Bytes})
}

// getAttachment serves an image; ids are content hashes, so the answer never changes.
func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request, _ store.Device) {
	id := r.PathValue("id")
	notFound := protocol.Errorf(protocol.CodeNotFound, "not found")
	if !store.ValidAttachmentID(id) {
		writeError(w, notFound, 0)
		return
	}
	atts, err := s.store.Attachments(r.Context(), []string{id})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, notFound, 0)
		return
	} else if err != nil {
		s.log.Error("reading attachment failed", "attachment_id", id, "err", err.Error())
		writeError(w, protocol.Errorf(protocol.CodeInternal, "internal error"), 0)
		return
	}
	f, err := s.store.OpenAttachment(id)
	if err != nil {
		s.log.Error("attachment file missing", "attachment_id", id, "err", err.Error())
		writeError(w, notFound, 0)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", atts[0].Mime)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+id+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", time.Time{}, f)
}

// withAttachments adds "attachments" to the user messages of one history page that relay runs with images produced:
// the session's image runs in created_at order, each matched to the first unmatched user message whose timestamp is
// within [created_at - 5 s, created_at + 120 s]. Every other field of a message is kept; on any doubt the page is
// returned as Hermes sent it.
// shortcut: the match is per page, so when a page starts right after an image run's message, that run may take a
// user message of the next 2 minutes; match on a run id if Hermes ever stores one with the message.
func (s *Server) withAttachments(ctx context.Context, sessionID string, body []byte) []byte {
	runs, err := s.store.SessionImageRuns(ctx, sessionID)
	if err != nil {
		s.log.Error("reading image runs failed", "session_id", sessionID, "err", err.Error())
		return body
	}
	if len(runs) == 0 {
		return body
	}
	var page map[string]json.RawMessage
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(body, &page) != nil || json.Unmarshal(page["data"], &msgs) != nil {
		return body
	}
	matched := make([]bool, len(msgs))
	changed := false
	for _, run := range runs {
		created := float64(run.CreatedAt) / 1000
		for i, m := range msgs {
			var role string
			var ts *float64 // nil: null or missing
			if matched[i] || json.Unmarshal(m["role"], &role) != nil || role != "user" ||
				json.Unmarshal(m["timestamp"], &ts) != nil || ts == nil || *ts < created-5 || *ts > created+120 {
				continue
			}
			list := make([]map[string]any, len(run.Attachments))
			for j, a := range run.Attachments {
				list[j] = map[string]any{"id": a.ID, "mime": a.Mime, "width": a.Width, "height": a.Height}
			}
			m["attachments"], _ = json.Marshal(list)
			matched[i], changed = true, true
			break
		}
	}
	if !changed {
		return body
	}
	page["data"], _ = json.Marshal(msgs)
	out, err := json.Marshal(page)
	if err != nil {
		return body
	}
	return out
}
