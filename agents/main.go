// Command trismegistus-agents lists, follows and stops Hermes' background work on the host, for the relay (which
// runs in a read-only container without host PIDs). It serves HTTP over a Unix socket; see README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	home, err := os.UserHomeDir()
	if err != nil {
		log.Error("no home directory", "err", err.Error())
		os.Exit(1)
	}
	sock := flag.String("socket", filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "trismegistus-agents", "agents.sock"),
		"Unix socket to serve on (its directory is made 0755, the socket 0666)")
	procs := flag.String("processes", filepath.Join(home, ".hermes", "processes.json"), "Hermes' process list")
	runs := flag.String("runs", filepath.Join(home, ".local", "state", "hermes-runs")+","+
		filepath.Join(home, ".hermes", "cache", "scratch", "tmac", "wt"),
		"comma-separated directories of Claude Code delegate runs (the first wins; the second is the old scratch folder)")
	hermes := flag.String("hermes", filepath.Join(home, ".local", "bin", "hermes"), "the hermes command (asked for its Python by rewinds)")
	hermesHome := flag.String("hermes-home", filepath.Join(home, ".hermes"), "HERMES_HOME for rewinds")
	hermesAgent := flag.String("hermes-agent", filepath.Join(home, ".hermes", "hermes-agent"), "Hermes' code, imported by rewinds")
	flag.Parse()

	h := &Helper{Processes: *procs, Runs: strings.Split(*runs, ","), Proc: "/proc", Home: home, Grace: 10 * time.Second, Now: time.Now, Log: log,
		StopUnit:    func(unit string) error { return exec.Command("systemctl", "--user", "stop", unit).Run() },
		ActiveUnits: activeUnits, Hermes: *hermes, HermesHome: *hermesHome, HermesAgent: *hermesAgent, RewindTimeout: 20 * time.Second}
	ln, err := listen(*sock)
	if err != nil {
		log.Error("listen failed", "socket", *sock, "err", err.Error())
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("serving", "socket", *sock)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve failed", "err", err.Error())
		os.Exit(1)
	}
}

// listen opens the socket so the relay's container user (another uid) can connect: directory 0755, socket 0666.
func listen(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("socket path must be absolute (is XDG_RUNTIME_DIR set?)")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSocket != 0 {
		_ = os.Remove(path) // a socket left by a crash
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// activeUnits lists the active dispatch-*.service user units; none when systemctl fails.
func activeUnits() map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", "--user", "list-units", "--plain", "--no-legend", "--no-pager", "--full",
		"dispatch-*.service").Output()
	return parseActiveUnits(string(out))
}

// parseActiveUnits reads `systemctl list-units --plain --no-legend` lines: unit, load, active, sub, description.
func parseActiveUnits(out string) map[string]bool {
	m := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 3 && (f[2] == "active" || f[2] == "activating") {
			m[f[0]] = true
		}
	}
	return m
}
