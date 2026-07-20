// Package cli implements the obol management CLI. Every verb dials the obold
// Unix socket and issues one framed wire request (internal/wire); the daemon is
// the single authority over budget state (decision #19). The CLI holds no budget
// state of its own.
package cli

import (
	"fmt"
	"net"
	"time"

	"github.com/scttfrdmn/obol/internal/wire"
)

// DefaultSocket matches obold's default listen path.
const DefaultSocket = "/run/obol/obold.sock"

// dialTimeout bounds connect + round-trip. The daemon answers in microseconds;
// this only fires when it is down or wedged.
const dialTimeout = 5 * time.Second

// roundTrip dials the socket, sends one request frame, and returns the one
// response frame. Each verb is a single request/response, so a fresh connection
// per call keeps the client stateless.
func roundTrip(socket string, req *wire.Frame) (*wire.Frame, error) {
	conn, err := net.DialTimeout("unix", socket, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("connect to obold at %s: %w", socket, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		return nil, err
	}
	if err := wire.WriteFrame(conn, req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	resp, err := wire.ReadFrame(conn)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return resp, nil
}
