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
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HermesAPIKey != "fake" || c.PublicURL != "https://relay.example" || len(c.TrustedProxies) != 2 ||
		c.TrustedProxies[0].String() != "10.0.0.1/32" || c.RequireServe() != nil || c.RequirePair() != nil {
		t.Fatalf("%+v", c)
	}
}

func TestBadValues(t *testing.T) {
	_, err := Load(env(map[string]string{
		"HERMES_URL": "ftp://x", "RELAY_ADDR": "nope", "PUBLIC_URL": "relay", "TRUSTED_PROXIES": "300.1.1.1/8", "LOG_LEVEL": "loud",
		"HERMES_API_KEY": "super-secret-key",
	}))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"HERMES_URL", "RELAY_ADDR", "PUBLIC_URL", "TRUSTED_PROXIES", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Fatal("config error leaks the key")
	}
}
