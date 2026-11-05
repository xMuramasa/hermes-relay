// Command trismegistus-term is the host helper behind the Mac app's terminal drawer (trismegistus#31): it owns the
// shells the relay streams to a device, one per connection on its Unix socket; the relay's container can't start host
// processes. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if os.Getuid() == 0 {
		log.Error("refusing to run as root: the socket hands out a shell as the user running this")
		os.Exit(1)
	}
	acct, err := lookupAccount("/etc/passwd", os.Getuid())
	if err != nil {
		log.Error("no passwd entry", "err", err.Error())
		os.Exit(1)
	}
	sock := flag.String("socket", filepath.Join(acct.Home, ".local", "state", "trismegistus-term", "term.sock"),
		"Unix socket to serve on (its directory is made 0750, the socket 0660)")
	flag.Parse()

	ln, err := listen(*sock)
	if err != nil {
		log.Error("listen failed", "socket", *sock, "err", err.Error())
		os.Exit(1)
	}
	host, _ := os.Hostname()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("serving", "socket", *sock)
	if err := NewHelper(acct, host, log).Serve(ctx, ln); err != nil {
		log.Error("serve failed", "err", err.Error())
		os.Exit(1)
	}
}

// listen opens the socket for this user and group only: it hands out a shell, and the group is how the relay's
// container gets in (group_add), so the modes are the security boundary. The umask covers the moment before accept.
func listen(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("socket path must be absolute")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSocket != 0 {
		_ = os.Remove(path) // a socket left by a crash
	}
	old := syscall.Umask(0o117)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	return ln, err
}
