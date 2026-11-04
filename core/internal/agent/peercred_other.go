//go:build !linux && !darwin

package agent

import (
	"errors"
	"net"
)

// peerUID is not supported on this platform: with a peer uid configured the
// agent refuses every connection.
func peerUID(*net.UnixConn) (int, error) {
	return -1, errors.New("peer credentials are not supported on this platform")
}
