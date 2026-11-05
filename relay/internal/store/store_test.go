package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
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

func TestRoles(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.CreatePairCodeWithRole(ctx, "mac", "admin", []byte("c0"), time.Minute, false, 5); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("bad role: %v", err)
	}
	if _, err := s.CreatePairCodeWithRole(ctx, "mac", RoleManager, []byte("c1"), time.Minute, false, 5); err != nil {
		t.Fatal(err)
	}
	if d, err := s.RedeemPairCode(ctx, []byte("c1"), []byte("t1"), 5); err != nil || d.Role != RoleManager {
		t.Fatalf("redeem manager: %+v %v", d, err)
	}
	if _, err := s.CreatePairCode(ctx, "phone", []byte("c2"), time.Minute, false, 5); err != nil {
		t.Fatal(err)
	}
	if d, err := s.RedeemPairCode(ctx, []byte("c2"), []byte("t2"), 5); err != nil || d.Role != RoleViewer {
		t.Fatalf("redeem default: %+v %v", d, err)
	}
	if d, _ := s.DeviceByTokenHash(ctx, []byte("t1")); d.Role != RoleManager {
		t.Fatalf("lookup: %+v", d)
	}
	if err := s.SetDeviceRole(ctx, "mac", RoleViewer); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeviceRole(ctx, "phone", RoleManager); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeviceRole(ctx, "nobody", RoleManager); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
	if err := s.SetDeviceRole(ctx, "mac", "root"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("bad role: %v", err)
	}
	list, _ := s.ListDevices(ctx)
	if len(list) != 2 || list[0].Role != RoleViewer || list[1].Role != RoleManager {
		t.Fatalf("list %+v", list)
	}
}

// A database from before roles keeps its devices (as viewers) and its unused pairing codes.
func TestMigrationToRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{migrations[0], `PRAGMA user_version = 1`,
		`INSERT INTO devices (name, token_hash, created_at) VALUES ('macbook', x'0102', 1)`,
		`INSERT INTO pair_codes (code_hash, name, created_at, expires_at) VALUES (x'0304', 'pixel', 1, 9999999999999)`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if d, err := s.DeviceByTokenHash(ctx, []byte{1, 2}); err != nil || d.Name != "macbook" || d.Role != RoleViewer {
		t.Fatalf("old device: %+v %v", d, err)
	}
	if d, err := s.RedeemPairCode(ctx, []byte{3, 4}, []byte{5, 6}, 5); err != nil || d.Role != RoleViewer {
		t.Fatalf("old code: %+v %v", d, err)
	}
	if err := s.SetDeviceRole(ctx, "macbook", RoleManager); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE devices SET role = 'root'`); err == nil {
		t.Fatal("the role column accepts unknown roles")
	}
}

func TestSessionTags(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if all, err := s.SessionTags(ctx); err != nil || all == nil || len(all) != 0 {
		t.Fatalf("empty: %v %v", all, err)
	}
	if err := s.SetSessionTags(ctx, "s1", []string{"Work", "ideas"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionTags(ctx, "s2", []string{"news"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionTags(ctx, "s1", []string{"zeta", "Alpha"}); err != nil {
		t.Fatal(err)
	}
	// Case-insensitive duplicates are refused (the server dedupes first) and leave the old set.
	if err := s.SetSessionTags(ctx, "s2", []string{"x", "X"}); err == nil {
		t.Fatal("a case-insensitive duplicate was stored")
	}
	all, err := s.SessionTags(ctx)
	if err != nil || len(all) != 2 || strings.Join(all["s1"], ",") != "zeta,Alpha" || strings.Join(all["s2"], ",") != "news" {
		t.Fatalf("after replace: %v %v", all, err)
	}
	if err := s.SetSessionTags(ctx, "s1", nil); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.SessionTags(ctx); len(all) != 1 || all["s1"] != nil {
		t.Fatalf("after drop: %v", all)
	}
}

// A database from before tags (version 2) keeps its devices and gets the tags table.
func TestMigrationToTags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{migrations[0], migrations[1], `PRAGMA user_version = 2`,
		`INSERT INTO devices (name, token_hash, created_at, role) VALUES ('macbook', x'0102', 1, 'manager')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if v, _ := s.Version(); v != len(migrations) {
		t.Fatalf("version %d", v)
	}
	if d, err := s.DeviceByTokenHash(ctx, []byte{1, 2}); err != nil || d.Role != RoleManager {
		t.Fatalf("old device: %+v %v", d, err)
	}
	if err := s.SetSessionTags(ctx, "s1", []string{"work"}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionLinks(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	links := func() string {
		t.Helper()
		all, err := s.SessionLinks(ctx)
		if err != nil || all == nil {
			t.Fatal(all, err)
		}
		var out []string
		for _, n := range []string{"b", "c", "d", "x"} {
			if o, ok := all[n]; ok {
				out = append(out, n+">"+o)
			}
		}
		return strings.Join(out, ",")
	}
	for _, l := range [][2]string{{"b", "a"}, {"c", "b"}, {"c", "b"}} { // the same link twice is fine
		if err := s.SetContinues(ctx, l[0], l[1], 32); err != nil {
			t.Fatal(l, err)
		}
	}
	for _, c := range []struct {
		newer, older string
		err          error
	}{
		{"d", "b", ErrLinkTaken}, {"a", "c", ErrLinkCycle}, {"a", "b", ErrLinkTaken}, {"d", "c", ErrChainTooLong},
	} {
		if err := s.SetContinues(ctx, c.newer, c.older, 2); !errors.Is(err, c.err) {
			t.Fatalf("%s>%s: %v, want %v", c.newer, c.older, err, c.err)
		}
	}
	if err := s.SetContinues(ctx, "x", "x", 32); err == nil {
		t.Fatal("a self-link was stored")
	}
	// Re-pointing a link frees its old target.
	if err := s.SetContinues(ctx, "c", "x", 32); err != nil || links() != "b>a,c>x" {
		t.Fatal(links(), err)
	}
	if err := s.SetContinues(ctx, "d", "b", 32); err != nil || links() != "b>a,c>x,d>b" {
		t.Fatal(links(), err)
	}
	// Deleting the middle of a>b>d keeps a and d, unlinked; the model choice goes too.
	if err := s.SetSessionModel(ctx, "b", SessionModel{"anthropic", "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionModel(ctx, "b", SessionModel{"copilot", "m2"}); err != nil {
		t.Fatal(err)
	}
	if m, err := s.SessionModels(ctx); err != nil || m["b"] != (SessionModel{"copilot", "m2"}) {
		t.Fatal(m, err)
	}
	if err := s.ForgetSession(ctx, "b"); err != nil || links() != "c>x" {
		t.Fatal(links(), err)
	}
	if m, _ := s.SessionModels(ctx); len(m) != 0 {
		t.Fatal(m)
	}
	if err := s.SetContinues(ctx, "c", "", 32); err != nil || links() != "" {
		t.Fatal(links(), err)
	}
}

// A database from before links (version 3) keeps its tags and gets the links and models tables.
func TestMigrationToLinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{migrations[0], migrations[1], migrations[2], `PRAGMA user_version = 3`,
		`INSERT INTO session_tags (session_id, tag) VALUES ('s1', 'work')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if v, _ := s.Version(); v != len(migrations) {
		t.Fatalf("version %d", v)
	}
	if tags, err := s.SessionTags(ctx); err != nil || tags["s1"][0] != "work" {
		t.Fatalf("old tags: %v %v", tags, err)
	}
	if err := s.SetContinues(ctx, "s2", "s1", 32); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionModel(ctx, "s2", SessionModel{"anthropic", "claude-opus-5-5[1m]"}); err != nil {
		t.Fatal(err)
	}
}

// A database from before the terminal grant (version 5) keeps its devices, with the grant off.
func TestMigrationToTerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{migrations[0], migrations[1], migrations[2], migrations[3], migrations[4], `PRAGMA user_version = 5`,
		`INSERT INTO devices (name, token_hash, created_at, role) VALUES ('macbook', x'0102', 1, 'manager')`,
		`INSERT INTO devices (name, token_hash, created_at, role) VALUES ('s21', x'0304', 1, 'viewer')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if v, _ := s.Version(); v != len(migrations) {
		t.Fatalf("version %d", v)
	}
	list, err := s.ListDevices(ctx)
	if err != nil || len(list) != 2 || list[0].Terminal || list[1].Terminal {
		t.Fatalf("devices: %+v %v", list, err)
	}
	allowed := func() map[int64]bool {
		t.Helper()
		ids, err := s.TerminalDevices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if ids := allowed(); len(ids) != 0 {
		t.Fatalf("no grant yet: %v", ids)
	}
	if err := s.SetDeviceTerminal(ctx, "macbook", true); err != nil {
		t.Fatal(err)
	}
	if d, err := s.DeviceByTokenHash(ctx, []byte{1, 2}); err != nil || !d.Terminal {
		t.Fatalf("%+v %v", d, err)
	}
	if ids := allowed(); !ids[list[0].ID] || len(ids) != 1 {
		t.Fatalf("granted manager: %v", ids)
	}
	if err := s.SetDeviceTerminal(ctx, "s21", true); !errors.Is(err, ErrNotManager) {
		t.Fatalf("a viewer can't get the grant: %v", err)
	}
	if err := s.SetDeviceTerminal(ctx, "s21", false); err != nil {
		t.Fatalf("off works on a viewer: %v", err)
	}
	if ids := allowed(); len(ids) != 1 {
		t.Fatalf("a viewer with the grant: %v", ids)
	}
	if err := s.SetDeviceRole(ctx, "macbook", RoleViewer); err != nil {
		t.Fatal(err)
	}
	if ids := allowed(); len(ids) != 0 {
		t.Fatalf("demoted: %v", ids)
	}
	// Promoted again, the grant doesn't come back by itself.
	if err := s.SetDeviceRole(ctx, "macbook", RoleManager); err != nil {
		t.Fatal(err)
	}
	if d, err := s.DeviceByTokenHash(ctx, []byte{1, 2}); err != nil || d.Terminal {
		t.Fatalf("a demotion drops the grant: %+v %v", d, err)
	}
	if err := s.SetDeviceTerminal(ctx, "nobody", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
}
