package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/scttfrdmn/obol/internal/budget"
	"github.com/scttfrdmn/obol/internal/wire"
)

func testNow() budget.Seconds { return 1000 }

// twoAccountConfig is a fixture: two accounts with distinct balances/rates and a
// window that contains testNow().
func twoAccountConfig() *Config {
	return &Config{Accounts: []AccountConfig{
		{Name: "lab_smith", Balance: 100000, Rate: 1, Window: "1000000s"},
		{Name: "lab_jones", Balance: 50000, Rate: 2, Window: "1000000s"},
	}}
}

func TestRegistryResolveAndRecover(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}

	smith, err := reg.Resolve("lab_smith")
	if err != nil {
		t.Fatal(err)
	}
	if smith.Balance() != 100000 {
		t.Errorf("smith balance = %d, want 100000", smith.Balance())
	}
	if _, err := reg.Resolve("lab_jones"); err != nil {
		t.Fatal(err)
	}
	// An unconfigured account does not resolve (multi-account: exact match only).
	if _, err := reg.Resolve("nope"); err == nil {
		t.Error("expected ErrNoBudget for unconfigured account")
	}

	// Spend on smith, then reopen the registry and confirm it recovers.
	if err := smith.Submit("j1", 100, 1000); err != nil {
		t.Fatal(err)
	}
	_ = reg.Close()

	reg2, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg2.Close() }()
	smith2, _ := reg2.Resolve("lab_smith")
	if smith2.Balance() != 100000-100 {
		t.Errorf("recovered smith balance = %d, want %d", smith2.Balance(), 100000-100)
	}
	// jones untouched.
	jones2, _ := reg2.Resolve("lab_jones")
	if jones2.Balance() != 50000 {
		t.Errorf("jones balance = %d, want 50000 (isolated from smith)", jones2.Balance())
	}
}

// serveReg starts an in-process server over a registry and returns a dial func.
func serveReg(t *testing.T, reg *Registry) func() net.Conn {
	t.Helper()
	srv := NewWithRegistry(reg, testNow, Weights{})
	// Keep the socket path SHORT: sockaddr_un caps at ~104 bytes, and a long test
	// name under t.TempDir()'s nested path overflows it. A shallow MkdirTemp dir
	// stays well under the limit.
	sockDir, err := os.MkdirTemp("", "obol")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close(); _ = reg.Close() })
	return func() net.Conn {
		c, e := net.Dial("unix", ln.Addr().String())
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
}

// TestGateRoutesToAccount confirms a gate for account X debits X's budget and
// leaves the other account untouched, and settle-by-jobid routes back correctly.
func TestGateRoutesToAccount(t *testing.T) {
	reg, err := NewRegistry(twoAccountConfig(), t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	dial := serveReg(t, reg)
	smith, _ := reg.Resolve("lab_smith")
	jones, _ := reg.Resolve("lab_jones")

	// Gate a 100s job under lab_smith: cost = rate(1)*100 = 100.
	resp := call(t, dial, wire.GateFrame(&wire.GateRequest{Account: "lab_smith", TimeLimit: 100, NTasks: 1}))
	if resp.GateResp == nil || !resp.GateResp.Allow {
		t.Fatalf("smith gate rejected: %+v", resp.GateResp)
	}
	token := resp.GateResp.Token
	if smith.Balance() != 100000-100 {
		t.Errorf("smith balance = %d, want %d", smith.Balance(), 100000-100)
	}
	if jones.Balance() != 50000 {
		t.Errorf("jones balance changed by a smith gate: %d", jones.Balance())
	}

	// Bind + settle by jobid must route back to smith.
	call(t, dial, wire.BindFrame(&wire.BindRequest{Token: token, JobID: "7"}))
	sr := call(t, dial, wire.SettleFrame(&wire.SettleRequest{JobID: "7", Kind: wire.SettleComplete, Runtime: 40}))
	if sr.SettleResp == nil || !sr.SettleResp.OK {
		t.Fatalf("settle failed: %+v", sr.SettleResp)
	}
	// Billed 40, refunded 60 -> smith back to 100000-40.
	if smith.Balance() != 100000-40 {
		t.Errorf("smith after settle = %d, want %d", smith.Balance(), 100000-40)
	}
	if ok, _ := smith.ConservationOK(); !ok {
		t.Error("smith conservation broken")
	}
	if ok, _ := jones.ConservationOK(); !ok {
		t.Error("jones conservation broken")
	}
}

// TestGateUnknownAccountRejected confirms a submission for an unconfigured
// account is rejected (SEAM §9: none resolves -> reject).
func TestGateUnknownAccountRejected(t *testing.T) {
	reg, err := NewRegistry(twoAccountConfig(), t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	dial := serveReg(t, reg)
	resp := call(t, dial, wire.GateFrame(&wire.GateRequest{Account: "ghost", TimeLimit: 100, NTasks: 1}))
	if resp.GateResp == nil || resp.GateResp.Allow {
		t.Fatalf("expected rejection for unknown account, got %+v", resp.GateResp)
	}
}

// TestStatusSelectsAccount confirms `show --account` selects the right budget and
// that omitting it with multiple accounts is an error.
func TestStatusSelectsAccount(t *testing.T) {
	reg, err := NewRegistry(twoAccountConfig(), t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	dial := serveReg(t, reg)

	resp := call(t, dial, wire.StatusFrame("lab_jones"))
	if resp.StatusResp == nil || !resp.StatusResp.OK {
		t.Fatalf("status lab_jones failed: %+v", resp.StatusResp)
	}
	if resp.StatusResp.B0 != 50000 || resp.StatusResp.Account != "lab_jones" {
		t.Errorf("status = %+v, want jones B0=50000", resp.StatusResp)
	}
	// No account with multiple configured -> error asking which.
	amb := call(t, dial, wire.StatusFrame(""))
	if amb.StatusResp == nil || amb.StatusResp.OK {
		t.Errorf("expected ambiguous-account error, got %+v", amb.StatusResp)
	}
}
