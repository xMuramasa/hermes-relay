package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func att(data string) (Attachment, []byte) {
	sum := sha256.Sum256([]byte(data))
	return Attachment{ID: hex.EncodeToString(sum[:]), Mime: "image/png", Bytes: int64(len(data)), Width: 2, Height: 1}, []byte(data)
}

func TestAttachments(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	s.SetClock(func() time.Time { return now })
	dir := filepath.Join(filepath.Dir(path), "attachments")
	exists := func(id string) bool { _, err := os.Stat(filepath.Join(dir, id)); return err == nil }

	a, aData := att("image a")
	b, bData := att("image b")
	c, cData := att("image c (never used)")
	for _, x := range []struct {
		a    Attachment
		data []byte
	}{{a, aData}, {b, bData}, {c, cData}} {
		if created, err := s.PutAttachment(ctx, x.a, x.data); err != nil || !created {
			t.Fatalf("put: %v %v", created, err)
		}
	}
	if created, err := s.PutAttachment(ctx, a, aData); err != nil || created {
		t.Fatalf("second put of the same bytes: created %v %v", created, err)
	}
	if got, _ := s.ReadAttachment(a.ID); string(got) != "image a" {
		t.Fatalf("read %q", got)
	}
	if _, err := s.Attachments(ctx, []string{a.ID, "0000000000000000000000000000000000000000000000000000000000000000"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := s.ReadAttachment("../relay.db"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id read: %v", err)
	}
	got, err := s.Attachments(ctx, []string{b.ID, a.ID})
	if err != nil || len(got) != 2 || got[0] != b || got[1] != a {
		t.Fatalf("attachments %+v %v", got, err)
	}

	// Two sessions share image a; session 1 has two runs with images.
	runs := []Run{
		{RunID: "run_1", SessionID: "s1", ClientMsgID: "m1", Status: "completed", Attachments: []string{a.ID, b.ID}},
		{RunID: "run_2", SessionID: "s1", ClientMsgID: "m2", Status: "completed", Attachments: []string{b.ID}},
		{RunID: "run_3", SessionID: "s2", ClientMsgID: "m3", Status: "completed", Attachments: []string{a.ID}},
		{RunID: "run_4", SessionID: "s1", ClientMsgID: "m4", Status: "completed"},
	}
	for _, r := range runs {
		if err := s.InsertRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if err := s.InsertRun(ctx, runs[0]); err != nil { // a resend's insert changes nothing
		t.Fatal(err)
	}
	ir, err := s.SessionImageRuns(ctx, "s1")
	if err != nil || len(ir) != 2 || ir[0].RunID != "run_1" || len(ir[0].Attachments) != 2 || ir[0].Attachments[0].ID != a.ID ||
		ir[0].Attachments[1].ID != b.ID || ir[1].RunID != "run_2" || ir[1].CreatedAt != ir[0].CreatedAt+1000 {
		t.Fatalf("image runs %+v %v", ir, err)
	}

	// A day later: the unclaimed upload is gone; runs with images stay, the other finished run doesn't.
	now = now.Add(25 * time.Hour)
	if _, err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(c.ID) || !exists(a.ID) || !exists(b.ID) {
		t.Fatalf("after cleanup: c %v a %v b %v", exists(c.ID), exists(a.ID), exists(b.ID))
	}
	if _, err := s.Attachments(ctx, []string{c.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unclaimed row: %v", err)
	}
	if _, err := s.RunByID(ctx, "run_4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished run without images should be cleaned: %v", err)
	}
	if ir, _ := s.SessionImageRuns(ctx, "s1"); len(ir) != 2 {
		t.Fatalf("image runs after cleanup: %+v", ir)
	}

	// Deleting session 1: b goes (only s1 used it), a stays for s2.
	if err := s.DropSessionAttachments(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if exists(b.ID) || !exists(a.ID) {
		t.Fatalf("after delete: a %v b %v", exists(a.ID), exists(b.ID))
	}
	if ir, _ := s.SessionImageRuns(ctx, "s1"); len(ir) != 0 {
		t.Fatalf("s1 still has image runs: %+v", ir)
	}
	if ir, _ := s.SessionImageRuns(ctx, "s2"); len(ir) != 1 {
		t.Fatalf("s2 lost its image: %+v", ir)
	}
	if err := s.DropSessionAttachments(ctx, "nothing-here"); err != nil {
		t.Fatal(err)
	}
}

// A fresh upload is kept for an hour; uploading the same bytes again restarts the hour.
func TestUnclaimedHour(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	s.SetClock(func() time.Time { return now })
	a, data := att("fresh")
	if _, err := s.PutAttachment(ctx, a, data); err != nil {
		t.Fatal(err)
	}
	now = now.Add(50 * time.Minute)
	if _, err := s.PutAttachment(ctx, a, data); err != nil {
		t.Fatal(err)
	}
	now = now.Add(50 * time.Minute)
	if _, err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attachments(ctx, []string{a.ID}); err != nil {
		t.Fatalf("re-uploaded attachment cleaned too early: %v", err)
	}
	now = now.Add(11 * time.Minute)
	if _, err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attachments(ctx, []string{a.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unclaimed after an hour: %v", err)
	}
}
