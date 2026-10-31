package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/pairing"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// TestRoles runs /api/me and a manager-only route over a server with only a store (neither touches Hermes).
func TestRoles(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := New(DefaultOptions(), st, nil, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pair := func(name, role string) string {
		_, codeHash := pairing.NewCode()
		if _, err := st.CreatePairCodeWithRole(ctx, name, role, codeHash, time.Minute, false, 5); err != nil {
			t.Fatal(err)
		}
		token, tokenHash := pairing.NewToken()
		if _, err := st.RedeemPairCode(ctx, codeHash, tokenHash, 5); err != nil {
			t.Fatal(err)
		}
		return token
	}
	mac, phone := pair("macbook", store.RoleManager), pair("pixel-8", store.RoleViewer)

	mux := http.NewServeMux()
	mux.Handle("/", s.Handler())
	mux.HandleFunc("POST /test/manage", s.authed(requireRole(store.RoleManager, func(w http.ResponseWriter, _ *http.Request, _ store.Device) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	do := func(method, path, token string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s %s: %s", method, path, b)
		}
		return resp.StatusCode, out
	}
	errCode := func(body map[string]any) string {
		e, _ := body["error"].(map[string]any)
		c, _ := e["code"].(string)
		return c
	}

	for token, want := range map[string]string{mac: "macbook manager", phone: "pixel-8 viewer"} {
		code, body := do("GET", "/api/me", token)
		if got := body["device"].(string) + " " + body["role"].(string); code != 200 || got != want || len(body) != 2 {
			t.Fatalf("/api/me: %d %v, want %s", code, body, want)
		}
	}
	if code, body := do("GET", "/api/me", ""); code != 401 || errCode(body) != "unauthorized" {
		t.Fatalf("/api/me without a token: %d %v", code, body)
	}
	if code, body := do("POST", "/test/manage", mac); code != 200 || body["ok"] != true {
		t.Fatalf("manager: %d %v", code, body)
	}
	if code, body := do("POST", "/test/manage", phone); code != 403 || errCode(body) != "forbidden" {
		t.Fatalf("viewer: %d %v", code, body)
	}
	if code, _ := do("POST", "/test/manage", ""); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	// set-role applies to the next request, no restart.
	if err := st.SetDeviceRole(ctx, "pixel-8", store.RoleManager); err != nil {
		t.Fatal(err)
	}
	if code, body := do("POST", "/test/manage", phone); code != 200 {
		t.Fatalf("promoted viewer: %d %v", code, body)
	}
}
