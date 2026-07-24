// Package cli implements the obol management CLI. Every verb dials the obold
// Unix socket and issues one framed wire request (internal/wire); the daemon is
// the single authority over budget state (decision #19). The CLI holds no budget
// state of its own.
package cli

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/scttfrdmn/obol/internal/wire"
)

// DefaultSocket matches obold's default listen path.
const DefaultSocket = "/run/obol/obold.sock"

// dialTimeout bounds connect + round-trip. The daemon answers in microseconds;
// this only fires when it is down or wedged.
const dialTimeout = 5 * time.Second

// resolveTarget decides where and how to reach obold. By default it's the local
// Unix socket (authorized by SO_PEERCRED). The off-host transport (#144 — e.g. a
// PCS login-node seam) is selected entirely via environment, so no verb grows a
// new flag:
//
//	OBOL_ADDR             host:port of a remote obold TCP listener
//	OBOL_AUTH_TOKEN       the bearer token, or
//	OBOL_AUTH_TOKEN_FILE  a file to read it from
//
// Returns (network, address, token). When OBOL_ADDR is unset, token is "" and the
// CLI dials the Unix socket exactly as before.
func resolveTarget(socket string) (network, address, token string, err error) {
	addr := strings.TrimSpace(os.Getenv("OBOL_ADDR"))
	if addr == "" {
		return "unix", socket, "", nil
	}
	tok := strings.TrimSpace(os.Getenv("OBOL_AUTH_TOKEN"))
	if tok == "" {
		if f := strings.TrimSpace(os.Getenv("OBOL_AUTH_TOKEN_FILE")); f != "" {
			b, rerr := os.ReadFile(f) //nolint:gosec // G304: operator supplies the path
			if rerr != nil {
				return "", "", "", fmt.Errorf("read OBOL_AUTH_TOKEN_FILE: %w", rerr)
			}
			tok = strings.TrimSpace(string(b))
		}
	}
	if tok == "" {
		return "", "", "", fmt.Errorf("OBOL_ADDR set but no token (set OBOL_AUTH_TOKEN or OBOL_AUTH_TOKEN_FILE)")
	}
	return "tcp", addr, tok, nil
}

// roundTrip dials the resolved target (local socket, or remote TCP with a token),
// sends one request frame, and returns the one response frame. Each verb is a
// single request/response, so a fresh connection per call keeps the client
// stateless. `socket` is the verb's --socket flag; OBOL_ADDR (if set) redirects
// to TCP and attaches the bearer token to the frame.
func roundTrip(socket string, req *wire.Frame) (*wire.Frame, error) {
	network, address, token, err := resolveTarget(socket)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Auth = token // authorize the off-host peer (#144)
	}
	conn, err := net.DialTimeout(network, address, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("connect to obold at %s: %w", address, err)
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
