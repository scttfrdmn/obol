package daemon

import (
	"net"
	"testing"
	"time"

	"github.com/scttfrdmn/obol/internal/budget"
	"github.com/scttfrdmn/obol/internal/wire"
)

// newTestTCPServer serves a budget over a TCP listener with an auth token set
// (#144), returning a dial func to 127.0.0.1:<port> and the token.
func newTestTCPServer(t *testing.T) (func() net.Conn, string) {
	t.Helper()
	dir := t.TempDir()
	bd, err := budget.NewDurable(dir, 1, 100000, 0, 100000, false)
	if err != nil {
		t.Fatalf("NewDurable: %v", err)
	}
	clk := &testClock{}
	srv := New(bd, clk.now)
	const token = "test-token-0123456789abcdef" // >= 16 chars
	srv.SetAuthToken(token)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = ln.Close()
		_ = bd.Close()
	})
	dial := func() net.Conn {
		conn, derr := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
		if derr != nil {
			t.Fatalf("dial tcp: %v", derr)
		}
		return conn
	}
	return dial, token
}

// TestTCPRequiresAuthToken: over TCP (no SO_PEERCRED), a request with no token or
// a wrong token is rejected; the correct token lets a gate through (#144).
func TestTCPRequiresAuthToken(t *testing.T) {
	dial, token := newTestTCPServer(t)

	gate := func(auth string) *wire.Frame {
		f := wire.GateFrame(&wire.GateRequest{Account: "lab", Partition: "cloud", TimeLimit: 100, NTasks: 1})
		f.Auth = auth
		return call(t, dial, f)
	}

	// No token → reject.
	if r := gate(""); r.GateResp == nil || r.GateResp.Allow {
		t.Errorf("no-token gate: want reject, got %+v", r.GateResp)
	}
	// Wrong token → reject.
	if r := gate("wrong-token-xxxxxxxxxxxx"); r.GateResp == nil || r.GateResp.Allow {
		t.Errorf("wrong-token gate: want reject, got %+v", r.GateResp)
	}
	// Correct token → allowed (escrow minted).
	r := gate(token)
	if r.GateResp == nil || !r.GateResp.Allow || r.GateResp.Token == "" {
		t.Errorf("valid-token gate: want allow with token, got %+v", r.GateResp)
	}
}

// TestTCPRefusesAdminVerbs: even with a valid token, a TCP peer cannot run admin
// mutating verbs — there is no SO_PEERCRED to authorize them (#144). They must be
// run over the local Unix socket.
func TestTCPRefusesAdminVerbs(t *testing.T) {
	dial, token := newTestTCPServer(t)

	// topup is admin-gated; over TCP it must be refused regardless of token.
	f := wire.TopUpFrame("lab", 100)
	f.Auth = token
	r := call(t, dial, f)
	if r.TopUpResp == nil || r.TopUpResp.OK {
		t.Errorf("topup over TCP: want refusal, got %+v", r.TopUpResp)
	}

	// A lifecycle verb (gate) with the same token still works — proving it's the
	// verb class, not the token, that's refused.
	g := wire.GateFrame(&wire.GateRequest{Account: "lab", Partition: "cloud", TimeLimit: 100, NTasks: 1})
	g.Auth = token
	if gr := call(t, dial, g); gr.GateResp == nil || !gr.GateResp.Allow {
		t.Errorf("gate over TCP with valid token: want allow, got %+v", gr.GateResp)
	}
}

// TestTCPNoTokenConfiguredRefusesAll: if the server has no auth token set, TCP is
// not enabled for auth and every remote request is refused (belt-and-suspenders:
// obold requires -auth-token-file with -listen, but the server enforces it too).
func TestTCPNoTokenConfiguredRefusesAll(t *testing.T) {
	dir := t.TempDir()
	bd, err := budget.NewDurable(dir, 1, 100000, 0, 100000, false)
	if err != nil {
		t.Fatalf("NewDurable: %v", err)
	}
	clk := &testClock{}
	srv := New(bd, clk.now) // no SetAuthToken
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close(); _ = bd.Close() })

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	f := wire.GateFrame(&wire.GateRequest{Account: "lab", Partition: "cloud", TimeLimit: 100, NTasks: 1})
	f.Auth = "anything-goes-here-1234567"
	if err := wire.WriteFrame(conn, f); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := wire.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.GateResp == nil || resp.GateResp.Allow {
		t.Errorf("gate on token-less TCP server: want reject, got %+v", resp.GateResp)
	}
}
