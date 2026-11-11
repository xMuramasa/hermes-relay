// Command trismegistus-relay sits between the Trismegistus app and Hermes' API server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/xMuramasa/trismegistus/relay/internal/agents"
	"github.com/xMuramasa/trismegistus/relay/internal/config"
	"github.com/xMuramasa/trismegistus/relay/internal/github"
	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/hub"
	"github.com/xMuramasa/trismegistus/relay/internal/pairing"
	"github.com/xMuramasa/trismegistus/relay/internal/server"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// version is stamped at build time: -ldflags "-X main.version=...".
var version = "dev"

const usage = `trismegistus-relay: relay between the Trismegistus app and Hermes.

Usage:
  trismegistus-relay [serve]                      run the relay (default)
  trismegistus-relay pair --name NAME [--replace] print a one-time pairing QR code for a new device
                     [--role manager|viewer]      (default viewer: chat only, no management)
  trismegistus-relay devices list                 list paired devices
  trismegistus-relay devices set-role NAME ROLE   make a device a manager or a viewer
  trismegistus-relay devices terminal NAME on|off grant a device the terminal, or take it away (its shells end)
  trismegistus-relay devices revoke NAME          revoke a device (its sockets close at once, its shells end)
  trismegistus-relay healthcheck                  GET /healthz on RELAY_ADDR; exit 0 if healthy
  trismegistus-relay version                      print the version

Environment:
  HERMES_API_KEY   Hermes API server key (required for serve)
  HERMES_URL       Hermes API server (default http://127.0.0.1:8642)
  RELAY_ADDR       listen address (default 127.0.0.1:8650)
  PUBLIC_URL       the relay's URL as the app sees it, put in the pairing QR (required for pair)
  DATA_DIR         directory for relay.db (default /data)
  TRUSTED_PROXIES  CIDRs whose X-Forwarded-For is honored (default 127.0.0.1/32,172.16.0.0/12)
  LOG_LEVEL        debug, info, warn or error (default info)
  GITHUB_TOKEN     GitHub token for the /api/github endpoints (optional; scopes repo + project)
  GITHUB_OWNER     owner whose repos the relay may reach (required with GITHUB_TOKEN)
  GITHUB_PROJECT   the board's ProjectV2 node id, PVT_... (required with GITHUB_TOKEN)
  AGENTS_SOCKET    Unix socket of the host helper trismegistus-agents, for /api/agents (optional)
  TERM_SOCKET      Unix socket of the host helper trismegistus-term, for /api/terminal (optional)
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "config: %v\n", err)
		return 2
	}
	switch cmd {
	case "serve":
		return serve(ctx, cfg, stdout, stderr)
	case "pair":
		return pair(ctx, cfg, args, stdout, stderr)
	case "devices":
		return devices(ctx, cfg, args, stdout, stderr)
	case "healthcheck":
		return healthcheck(cfg, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

func openStore(cfg config.Config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	return store.Open(cfg.DBPath())
}

func newLogger(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}

func serve(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) int {
	if err := cfg.RequireServe(); err != nil {
		fmt.Fprintf(stderr, "config: %v\n", err)
		return 2
	}
	log := newLogger(stdout, cfg.LogLevel)
	st, err := openStore(cfg)
	if err != nil {
		log.Error("opening database failed", "err", err.Error())
		return 1
	}
	defer st.Close()
	hc, err := hermes.New(cfg.HermesURL, cfg.HermesAPIKey)
	if err != nil {
		log.Error("bad Hermes URL")
		return 2
	}
	h := hub.New(st, hc, log, hub.DefaultOptions())
	opts := server.DefaultOptions()
	opts.Version, opts.TrustedProxies = version, cfg.TrustedProxies
	srv, err := server.New(opts, st, h, hc, log)
	if err != nil {
		log.Error("server setup failed", "err", err.Error())
		return 1
	}
	if cfg.GitHubToken != "" {
		srv.SetGitHub(github.New(config.DefaultGitHubAPIURL, cfg.GitHubToken, cfg.GitHubOwner, cfg.GitHubProject))
	}
	if cfg.AgentsSocket != "" {
		ac := agents.New(cfg.AgentsSocket)
		srv.SetAgents(ac)
		h.SetAgents(ac)
	}
	srv.SetTerminal(cfg.TermSocket)
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Error("listen failed", "addr", cfg.Addr, "err", err.Error())
		return 1
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := h.Start(ctx); err != nil {
		log.Error("resuming runs failed", "err", err.Error())
		return 1
	}
	bg, bgCancel := context.WithCancel(ctx)
	defer bgCancel()
	go srv.Run(bg)
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	log.Info("relay started", "version", version, "addr", ln.Addr().String(), "public_url", cfg.PublicURL)

	code := 0
	select {
	case <-ctx.Done():
	case err := <-errc:
		log.Error("http server failed", "err", err.Error())
		code = 1
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(sctx)
	srv.CloseSockets()
	h.Close()
	log.Info("stopped")
	return code
}

func pair(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "device name (letters, digits, . _ -)")
	replace := fs.Bool("replace", false, "revoke an existing device with this name")
	role := fs.String("role", store.RoleViewer, "manager or viewer")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return 2
	}
	if !store.ValidRole(*role) {
		fmt.Fprintln(stderr, "pair: --role must be manager or viewer")
		return 2
	}
	if !pairing.ValidName(*name) {
		fmt.Fprintln(stderr, "pair: --name is required: 1-64 letters, digits, '.', '_' or '-'")
		return 2
	}
	if err := cfg.RequirePair(); err != nil {
		fmt.Fprintf(stderr, "pair: %v\n", err)
		return 2
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "pair: %v\n", err)
		return 1
	}
	defer st.Close()
	code, hash := pairing.NewCode()
	revoked, err := st.CreatePairCodeWithRole(ctx, *name, *role, hash, pairing.CodeTTL, *replace, server.DefaultOptions().MaxDevices)
	switch {
	case errors.Is(err, store.ErrNameExists):
		fmt.Fprintf(stderr, "pair: a device named %q exists; use --replace to revoke it and pair again\n", *name)
		return 1
	case errors.Is(err, store.ErrTooManyDevices):
		fmt.Fprintln(stderr, "pair: device limit reached; revoke one first (devices revoke NAME)")
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "pair: %v\n", err)
		return 1
	}
	if revoked {
		fmt.Fprintf(stdout, "Revoked the old device %q.\n", *name)
	}
	pairing.Print(stdout, *name, cfg.PublicURL, code, time.Now().Add(pairing.CodeTTL))
	fmt.Fprintf(stdout, "Role:    %s\n", *role)
	return 0
}

const devicesUsage = "devices: use 'devices list', 'devices set-role NAME manager|viewer', 'devices terminal NAME on|off' or 'devices revoke NAME'\n"

func devices(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, devicesUsage)
		return 2
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "devices: %v\n", err)
		return 1
	}
	defer st.Close()
	switch {
	case args[0] == "list" && len(args) == 1:
		list, err := st.ListDevices(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "devices: %v\n", err)
			return 1
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tROLE\tTERMINAL\tPAIRED\tLAST SEEN")
		for _, d := range list {
			seen := "never"
			if !d.LastSeenAt.IsZero() {
				seen = d.LastSeenAt.Local().Format(time.DateTime)
			}
			terminal := "off"
			if d.Terminal {
				terminal = "on"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Name, d.Role, terminal, d.CreatedAt.Local().Format(time.DateTime), seen)
		}
		_ = tw.Flush()
		return 0
	case args[0] == "set-role" && len(args) == 3:
		switch err := st.SetDeviceRole(ctx, args[1], args[2]); {
		case errors.Is(err, store.ErrInvalidRole):
			fmt.Fprintln(stderr, "devices: role must be manager or viewer")
			return 2
		case errors.Is(err, store.ErrNotFound):
			fmt.Fprintf(stderr, "devices: no device named %q\n", args[1])
			return 1
		case err != nil:
			fmt.Fprintf(stderr, "devices: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%q is now a %s. It applies to its next request.\n", args[1], args[2])
		return 0
	case args[0] == "terminal" && len(args) == 3 && (args[2] == "on" || args[2] == "off"):
		if err := st.SetDeviceTerminal(ctx, args[1], args[2] == "on"); errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "devices: no device named %q\n", args[1])
			return 1
		} else if errors.Is(err, store.ErrNotManager) {
			fmt.Fprintf(stderr, "devices: %q is a viewer; make it a manager first (devices set-role %s manager)\n", args[1], args[1])
			return 1
		} else if err != nil {
			fmt.Fprintf(stderr, "devices: %v\n", err)
			return 1
		}
		if args[2] == "on" {
			fmt.Fprintf(stdout, "Terminal on for %q: as a manager it can open a shell on this host.\n", args[1])
		} else {
			fmt.Fprintf(stdout, "Terminal off for %q. Its open terminals close within seconds.\n", args[1])
		}
		return 0
	case args[0] == "revoke" && len(args) == 2:
		if err := st.RevokeDevice(ctx, args[1]); errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "devices: no device named %q\n", args[1])
			return 1
		} else if err != nil {
			fmt.Fprintf(stderr, "devices: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "Revoked %q. Its sockets and terminals close within seconds.\n", args[1])
		return 0
	default:
		fmt.Fprint(stderr, devicesUsage)
		return 2
	}
}

func healthcheck(cfg config.Config, stderr io.Writer) int {
	host, port, _ := net.SplitHostPort(cfg.Addr)
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(stderr, "unhealthy: relay not reachable")
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "unhealthy: HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
