package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestMigrationsAndWAL(t *testing.T) {
	s, path := open(t)
	if v, err := s.Version(); err != nil || v != len(migrations) {
		t.Fatalf("version %d %v", v, err)
	}
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode %q %v", mode, err)
	}
	// A second process (the CLI) opens the same file while the first is open.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("a database from a newer relay should be refused")
	}
}

func TestPairCodes(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	code, tok := []byte("code-hash-1"), []byte("token-hash-1")
	if _, err := s.CreatePairCode(ctx, "pixel", code, 10*time.Minute, false, 5); err != nil {
		t.Fatal(err)
	}
	d, err := s.RedeemPairCode(ctx, code, tok, 5)
	if err != nil || d.Name != "pixel" {
		t.Fatalf("%v %v", d, err)
	}
	if _, err := s.RedeemPairCode(ctx, code, []byte("token-hash-2"), 5); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("second redeem: %v", err)
	}
	got, err := s.DeviceByTokenHash(ctx, tok)
	if err != nil || got.ID != d.ID {
		t.Fatalf("lookup %v %v", got, err)
	}
	if _, err := s.DeviceByTokenHash(ctx, []byte("nope")); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// Name exists, then --replace.
	if _, err := s.CreatePairCode(ctx, "pixel", []byte("c2"), time.Minute, false, 5); !errors.Is(err, ErrNameExists) {
		t.Fatalf("name exists: %v", err)
	}
	revoked, err := s.CreatePairCode(ctx, "pixel", []byte("c2"), time.Minute, true, 5)
	if err != nil || !revoked {
		t.Fatalf("replace: %v %v", revoked, err)
	}
	if _, err := s.DeviceByTokenHash(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Fatal("replaced device still valid")
	}
	// Expired.
	if _, err := s.CreatePairCode(ctx, "old", []byte("c3"), -time.Second, false, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemPairCode(ctx, []byte("c3"), []byte("t3"), 5); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expired: %v", err)
	}
	// Device limit.
	for i, n := range []string{"a", "b"} {
		h := []byte{byte(i), 'x'}
		if _, err := s.CreatePairCode(ctx, n, h, time.Minute, false, 5); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RedeemPairCode(ctx, h, []byte{byte(i), 't'}, 5); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreatePairCode(ctx, "c", []byte("c4"), time.Minute, false, 2); !errors.Is(err, ErrTooManyDevices) {
		t.Fatalf("limit at creation: %v", err)
	}
	if _, err := s.CreatePairCode(ctx, "c", []byte("c4"), time.Minute, false, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemPairCode(ctx, []byte("c4"), []byte("t4"), 2); !errors.Is(err, ErrTooManyDevices) {
		t.Fatalf("limit at redeem: %v", err)
	}
	if _, err := s.RedeemPairCode(ctx, []byte("c4"), []byte("t4"), 5); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("a refused code must stay used: %v", err)
	}
	list, _ := s.ListDevices(ctx)
	if len(list) != 2 || list[0].Name != "a" {
		t.Fatalf("list %v", list)
	}
	if err := s.RevokeDevice(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeDevice(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.TouchDevice(ctx, list[1].ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListDevices(ctx); list[0].LastSeenAt.IsZero() {
		t.Fatal("last seen not recorded")
	}
}

func TestEventsNumberingReplayAndCleanup(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.InsertRun(ctx, Run{RunID: "run_1", SessionID: "s1", ClientMsgID: "c1", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	add := func(n int, hseq int64, status string) []Event {
		evs := make([]NewEvent, n)
		for i := range evs {
			evs[i] = NewEvent{Kind: "tool.started", Data: []byte(`{"tool":"t"}`), TS: 1}
		}
		out, err := s.AppendEvents(ctx, "s1", "run_1", evs, RunUpdate{HermesSeq: hseq, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := add(2, 0, ""); out[0].Seq != 1 || out[1].Seq != 2 {
		t.Fatalf("seqs %v", out)
	}
	add(1, 3, "")
	add(0, 1, "waiting_for_approval") // lower hermes seq never goes back
	r, err := s.RunByClientMsgID(ctx, "c1")
	if err != nil || r.HermesSeq != 3 || r.Status != "waiting_for_approval" {
		t.Fatalf("run %+v %v", r, err)
	}
	if act, _ := s.ActiveRuns(ctx); len(act) != 1 {
		t.Fatalf("active %v", act)
	}
	evs, err := s.EventsAfter(ctx, "s1", 1, 100)
	if err != nil || len(evs) != 2 || evs[0].Seq != 2 {
		t.Fatalf("after 1: %v %v", evs, err)
	}
	if _, err := s.EventsAfter(ctx, "s1", 0, 2); !errors.Is(err, ErrReplayGap) {
		t.Fatal("over the replay limit should be a gap")
	}
	if _, err := s.EventsAfter(ctx, "s1", 4, 100); !errors.Is(err, ErrReplayGap) {
		t.Fatal("beyond the last seq should be a gap")
	}
	if evs, err := s.EventsAfter(ctx, "unknown", 0, 100); err != nil || len(evs) != 0 {
		t.Fatalf("unknown session: %v %v", evs, err)
	}
	add(0, -1, "completed")
	if act, _ := s.ActiveRuns(ctx); len(act) != 0 {
		t.Fatal("completed run still active")
	}

	s.SetClock(func() time.Time { return time.Now().Add(25 * time.Hour) })
	n, err := s.Cleanup(ctx)
	if err != nil || n != 3 {
		t.Fatalf("cleanup %d %v", n, err)
	}
	if _, err := s.EventsAfter(ctx, "s1", 0, 100); !errors.Is(err, ErrReplayGap) {
		t.Fatal("pruned events should be a gap")
	}
	if evs, err := s.EventsAfter(ctx, "s1", 3, 100); err != nil || len(evs) != 0 {
		t.Fatalf("after the pruned seq: %v %v", evs, err)
	}
	if _, err := s.RunByID(ctx, "run_1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("old finished run not cleaned up")
	}
	if out := add(1, -1, ""); out[0].Seq != 4 {
		t.Fatalf("numbering must continue after cleanup, got %d", out[0].Seq)
	}
}
