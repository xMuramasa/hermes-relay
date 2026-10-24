// Package config reads the relay's settings from the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
)

// Config is the relay's configuration.
type Config struct {
	HermesURL      string
	HermesAPIKey   string
	Addr           string
	PublicURL      string
	DataDir        string
	TrustedProxies []netip.Prefix
	LogLevel       string
}

// DBPath is the SQLite file inside DataDir.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "relay.db") }

// Defaults.
const (
	DefaultHermesURL      = "http://127.0.0.1:8642"
	DefaultAddr           = "127.0.0.1:8650"
	DefaultDataDir        = "/data"
	DefaultTrustedProxies = "127.0.0.1/32,172.16.0.0/12"
)

// Load reads the environment through getenv. Only the settings every subcommand needs are checked here; serve
// also calls RequireServe.
func Load(getenv func(string) string) (Config, error) {
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		HermesURL:    get("HERMES_URL", DefaultHermesURL),
		HermesAPIKey: strings.TrimSpace(getenv("HERMES_API_KEY")),
		Addr:         get("RELAY_ADDR", DefaultAddr),
		PublicURL:    strings.TrimRight(get("PUBLIC_URL", ""), "/"),
		DataDir:      get("DATA_DIR", DefaultDataDir),
		LogLevel:     strings.ToLower(get("LOG_LEVEL", "info")),
	}
	var errs []error
	if u, err := url.Parse(c.HermesURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("HERMES_URL must be an http(s) URL"))
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		errs = append(errs, errors.New("RELAY_ADDR must be host:port"))
	}
	if c.PublicURL != "" {
		if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("PUBLIC_URL must be an http(s) URL"))
		}
	}
	for _, p := range strings.Split(get("TRUSTED_PROXIES", DefaultTrustedProxies), ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pfx, err := netip.ParsePrefix(p)
		if err != nil {
			if a, aerr := netip.ParseAddr(p); aerr == nil {
				pfx = netip.PrefixFrom(a, a.BitLen())
			} else {
				errs = append(errs, fmt.Errorf("TRUSTED_PROXIES: bad entry %q", p))
				continue
			}
		}
		c.TrustedProxies = append(c.TrustedProxies, pfx.Masked())
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, errors.New("LOG_LEVEL must be debug, info, warn or error"))
	}
	return c, errors.Join(errs...)
}

// RequireServe checks what serve needs on top of Load.
func (c Config) RequireServe() error {
	if c.HermesAPIKey == "" {
		return errors.New("HERMES_API_KEY is required")
	}
	return nil
}

// RequirePair checks what pair needs on top of Load.
func (c Config) RequirePair() error {
	if c.PublicURL == "" {
		return errors.New("PUBLIC_URL is required to pair (it goes in the QR code)")
	}
	return nil
}
