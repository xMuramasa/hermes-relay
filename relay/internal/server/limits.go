package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// limiter allows max events per key in a sliding window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string][]time.Time{}}
}

// Allow records one event for key and reports whether it's within the limit.
func (l *limiter) Allow(key string) bool {
	now := time.Now()
	cut := now.Add(-l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 10000 {
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(cut) {
				delete(l.hits, k)
			}
		}
	}
	h := l.hits[key]
	i := 0
	for i < len(h) && h[i].Before(cut) {
		i++
	}
	h = h[i:]
	if len(h) >= l.max {
		l.hits[key] = h
		return false
	}
	l.hits[key] = append(h, now)
	return true
}

// clientIP is the peer address, or, when the peer is a trusted proxy, the rightmost X-Forwarded-For entry that
// isn't one.
func clientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	if !isTrusted(peer) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	ip := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		ip = a.Unmap()
		if !isTrusted(ip) {
			break
		}
	}
	return ip
}
