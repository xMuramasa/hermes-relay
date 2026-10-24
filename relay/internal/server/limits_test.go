package server

import (
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("172.16.0.0/12")}
	cases := []struct {
		peer, xff, want string
	}{
		{"203.0.113.5:1234", "", "203.0.113.5"},
		{"203.0.113.5:1234", "1.2.3.4", "203.0.113.5"},                // untrusted peer: header ignored
		{"127.0.0.1:1234", "198.51.100.9", "198.51.100.9"},            // Caddy on the host
		{"172.18.0.2:1234", "10.0.0.1, 198.51.100.9", "198.51.100.9"}, // rightmost untrusted hop
		{"172.18.0.2:1234", "198.51.100.9, 172.18.0.5", "198.51.100.9"},
		{"172.18.0.2:1234", "junk", "172.18.0.2"},
		{"127.0.0.1:1234", "", "127.0.0.1"},
		{"[::ffff:127.0.0.1]:1234", "198.51.100.1", "198.51.100.1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/api/pair", nil)
		r.RemoteAddr = c.peer
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := clientIP(r, trusted).String(); got != c.want {
			t.Errorf("%s / %q: %s, want %s", c.peer, c.xff, got, c.want)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(2, 50*time.Millisecond)
	if !l.Allow("a") || !l.Allow("a") || l.Allow("a") || !l.Allow("b") {
		t.Fatal("limit")
	}
	time.Sleep(60 * time.Millisecond)
	if !l.Allow("a") {
		t.Fatal("window did not slide")
	}
}
