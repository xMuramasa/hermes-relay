package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.HermesURL != DefaultHermesURL || c.Addr != DefaultAddr || c.DataDir != "/data" || c.DBPath() != "/data/relay.db" ||
		len(c.TrustedProxies) != 2 || c.LogLevel != "info" {
		t.Fatalf("%+v", c)
	}
	if c.RequireServe() == nil {
		t.Fatal("serve needs HERMES_API_KEY")
	}
	if c.RequirePair() == nil {
		t.Fatal("pair needs PUBLIC_URL")
	}
}

func TestValues(t *testing.T) {
	c, err := Load(env(map[string]string{
		"HERMES_URL": "http://127.0.0.1:9999", "HERMES_API_KEY": " fake ", "RELAY_ADDR": "0.0.0.0:1",
		"PUBLIC_URL": "https://relay.example/", "DATA_DIR": "/tmp/x", "TRUSTED_PROXIES": "10.0.0.1, 192.168.0.0/16",
		"AGENTS_SOCKET": "/run/agents/agents.sock",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HermesAPIKey != "fake" || c.PublicURL != "https://relay.example" || len(c.TrustedProxies) != 2 ||
		c.TrustedProxies[0].String() != "10.0.0.1/32" || c.RequireServe() != nil || c.RequirePair() != nil ||
		c.AgentsSocket != "/run/agents/agents.sock" {
		t.Fatalf("%+v", c)
	}
}

func TestBadValues(t *testing.T) {
	_, err := Load(env(map[string]string{
		"HERMES_URL": "ftp://x", "RELAY_ADDR": "nope", "PUBLIC_URL": "relay", "TRUSTED_PROXIES": "300.1.1.1/8", "LOG_LEVEL": "loud",
		"AGENTS_SOCKET":  "agents.sock",
		"TERM_SOCKET":    "term.sock",
		"HERMES_API_KEY": "super-secret-key",
	}))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"HERMES_URL", "RELAY_ADDR", "PUBLIC_URL", "TRUSTED_PROXIES", "LOG_LEVEL", "AGENTS_SOCKET", "TERM_SOCKET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Fatal("config error leaks the key")
	}
}

func TestGitHub(t *testing.T) {
	with := func(kv ...string) Config {
		m := map[string]string{"HERMES_API_KEY": "k"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		c, err := Load(env(m))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := with(); c.GitHubToken != "" || c.RequireServe() != nil {
		t.Fatalf("no GitHub settings is fine (not_configured): %+v", c)
	}
	if c := with("GITHUB_OWNER", "xMuramasa", "GITHUB_PROJECT", "PVT_x"); c.RequireServe() != nil {
		t.Fatal("owner and project without a token is fine (not_configured)")
	}
	c := with("GITHUB_TOKEN", " ghp_secret ", "GITHUB_OWNER", "xMuramasa", "GITHUB_PROJECT", "PVT_kwHOAbc")
	if c.GitHubToken != "ghp_secret" || c.GitHubOwner != "xMuramasa" || c.GitHubProject != "PVT_kwHOAbc" || c.RequireServe() != nil {
		t.Fatalf("%+v", c)
	}
	for _, kv := range [][]string{
		{"GITHUB_TOKEN", "ghp_secret"},
		{"GITHUB_TOKEN", "ghp_secret", "GITHUB_OWNER", "xMuramasa"},
		{"GITHUB_TOKEN", "ghp_secret", "GITHUB_PROJECT", "PVT_x"},
		{"GITHUB_TOKEN", "ghp_secret", "GITHUB_OWNER", "x/y", "GITHUB_PROJECT", "PVT_x"},
		{"GITHUB_TOKEN", "ghp_secret", "GITHUB_OWNER", "xMuramasa", "GITHUB_PROJECT", "12"},
	} {
		err := with(kv...).RequireServe()
		if err == nil || !strings.Contains(err.Error(), "GITHUB_OWNER") || strings.Contains(err.Error(), "ghp_secret") {
			t.Fatalf("%v: %v", kv, err)
		}
	}
}
