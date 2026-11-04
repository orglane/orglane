package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
)

// listen creates the agent socket: its directory mode 0750 and the socket
// mode 0660, both with the group of the core's uid when the agent runs as
// root, so the core can connect and other unprivileged processes cannot
// (docs/specs/agent.md "Transport and authentication"). A stale socket is
// removed. With peerUID >= 0, connections from other uids are closed before
// a byte is read; the agent's own uid is allowed too (the container
// healthcheck runs as the agent's user).
func listen(ctx context.Context, path string, peerUID int, log *slog.Logger) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("agent socket: %w", err)
	}
	// The directory's mode and owner are changed below, so it must belong to
	// the agent alone (by default the agent-run volume): a shared directory
	// such as /tmp is refused.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("agent socket: %w", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			return nil, fmt.Errorf("agent socket: %s must be a directory for the agent socket only, but it contains %s", dir, e.Name())
		}
	}
	if err := os.Chmod(dir, 0o750); err != nil { //nolint:gosec // the group is the core's
		return nil, fmt.Errorf("agent socket: %w", err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("agent socket: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("agent socket: remove stale socket: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("agent socket: %w", err)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("agent socket: %w", err)
	}
	if err := os.Chmod(path, 0o660); err != nil { //nolint:gosec // the group is the core's
		_ = ln.Close()
		return nil, fmt.Errorf("agent socket: %w", err)
	}
	if peerUID >= 0 && os.Geteuid() == 0 {
		for _, p := range []string{dir, path} {
			if err := os.Chown(p, 0, peerUID); err != nil {
				_ = ln.Close()
				return nil, fmt.Errorf("agent socket: %w", err)
			}
		}
	}
	if peerUID < 0 {
		log.Warn("the peer uid check is disabled (agent.peer_uid = -1); only signed requests protect the socket")
		return ln, nil
	}
	return &peerListener{Listener: ln, allowed: []int{peerUID, os.Geteuid()}, log: log}, nil
}

// peerListener closes connections from processes with another uid.
type peerListener struct {
	net.Listener
	allowed []int
	log     *slog.Logger
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			_ = c.Close()
			continue
		}
		uid, err := peerUID(uc)
		if err != nil || !slices.Contains(l.allowed, uid) {
			l.log.Warn("refused a connection from another uid", "uid", uid, "error", err)
			_ = c.Close()
			continue
		}
		return c, nil
	}
}
