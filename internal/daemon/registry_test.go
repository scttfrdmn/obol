package daemon

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
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

// TestRegistryCreatePersistsAndDiscovers covers runtime create + restart
// discovery: create an account, reopen a fresh registry over the same state dir
// (with a DIFFERENT config), and confirm the created account survived with its
// balance and access — proving per-account state is the source of truth.
func TestRegistryCreatePersistsAndDiscovers(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// Create a third account at runtime.
	if err := reg.Create(AccountConfig{Name: "lab_new", Balance: 7000, Rate: 3, Window: "1000000s", AllowUsers: []string{"zoe"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Dup rejected.
	if err := reg.Create(AccountConfig{Name: "lab_new", Balance: 1, Rate: 1}); !errors.Is(err, ErrExists) {
		t.Errorf("dup create = %v, want ErrExists", err)
	}
	nb, _ := reg.Resolve("lab_new")
	if nb.Balance() != 7000 {
		t.Errorf("created balance = %d, want 7000", nb.Balance())
	}
	_ = reg.Close()

	// Reopen with a config that does NOT mention lab_new; discovery must find it.
	reg2, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg2.Close() }()
	nb2, err := reg2.Resolve("lab_new")
	if err != nil {
		t.Fatalf("lab_new not discovered after restart: %v", err)
	}
	if nb2.Balance() != 7000 {
		t.Errorf("discovered balance = %d, want 7000", nb2.Balance())
	}
	// Access survived via account.json.
	ac, ok := reg2.accessOf("lab_new")
	if !ok || len(ac.AllowUsers) != 1 || ac.AllowUsers[0] != "zoe" {
		t.Errorf("discovered access = %+v, want AllowUsers=[zoe]", ac)
	}
}

// burstConfigForTest returns a config with one burst-enabled account.
func burstAccountConfig() *Config {
	return &Config{Accounts: []AccountConfig{
		{Name: "burstlab", Balance: 100000, Rate: 1, Window: "1000000s",
			BurstEnabled: true, BurstCeilingPct: 0.5, BurstDrawCap: 2000},
	}}
}

// TestRegistryBurstConfigSurvivesRestart is the ordering-bug regression test.
// Burst config is persisted ONLY in the snapshot (no WAL command), and obold does
// not snapshot on shutdown — so if registry.create enabled burst AFTER the
// initial offset-0 snapshot, a restart with no intervening mutation would reload
// a snapshot that says "disabled" and burst would silently vanish. NewDurableBurst
// sets it BEFORE the initial snapshot; this test proves the config reaches the
// budget AND survives a reopen unchanged.
func TestRegistryBurstConfigSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistry(burstAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	bd, err := reg.Resolve("burstlab")
	if err != nil {
		t.Fatal(err)
	}
	if r := bd.Report(testNow()); !r.BurstEnabled || r.BurstCeiling != 50000 { // 0.5 * 100000
		t.Fatalf("burst not enabled at create: enabled=%v ceiling=%d", r.BurstEnabled, r.BurstCeiling)
	}
	_ = reg.Close() // NB: no snapshot on close, and no mutation happened — the
	// offset-0 snapshot is the ONLY persisted state.

	// Reopen via discovery (config path doesn't re-create; on-disk state wins).
	reg2, err := NewRegistry(burstAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg2.Close() }()
	bd2, err := reg2.Resolve("burstlab")
	if err != nil {
		t.Fatal(err)
	}
	r := bd2.Report(testNow())
	if !r.BurstEnabled {
		t.Fatal("burst DISABLED after restart — config lost (ordering bug)")
	}
	if r.BurstCeiling != 50000 {
		t.Errorf("recovered burst ceiling = %d, want 50000", r.BurstCeiling)
	}
}

// TestRegistrySweepUnbound confirms the registry-wide unbound-token sweep reaches
// every account's budget and reclaims stale never-started escrows.
func TestRegistrySweepUnbound(t *testing.T) {
	reg, err := NewRegistry(twoAccountConfig(), t.TempDir(), false, testNow) // clock fixed at 1000
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	smith, _ := reg.Resolve("lab_smith")
	jones, _ := reg.Resolve("lab_jones")

	// Escrow one unbound token in each account at t=1000 (the fixed testNow).
	if err := smith.SubmitAt("tok-s", 1, 100, testNow()); err != nil {
		t.Fatal(err)
	}
	if err := jones.SubmitAt("tok-j", 2, 100, testNow()); err != nil {
		t.Fatal(err)
	}
	sBal, jBal := smith.Balance(), jones.Balance()

	// Sweep with now well past submit+ttl: both reclaimed.
	swept := reg.SweepUnbound(300, testNow()+1000)
	if swept != 2 {
		t.Fatalf("registry sweep = %d, want 2 (one per account)", swept)
	}
	if smith.Balance() <= sBal || jones.Balance() <= jBal {
		t.Errorf("balances not refunded: smith %d->%d jones %d->%d", sBal, smith.Balance(), jBal, jones.Balance())
	}
	if ok, _ := smith.ConservationOK(); !ok {
		t.Error("smith conservation broken after sweep")
	}
}

// TestRegistrySetAccessPersists covers attach/detach persistence.
func TestRegistrySetAccessPersists(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SetAccess("lab_smith", []string{"alice"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetAccess("ghost", []string{"x"}, nil); !errors.Is(err, ErrNoBudget) {
		t.Errorf("SetAccess unknown = %v, want ErrNoBudget", err)
	}
	_ = reg.Close()

	reg2, err := NewRegistry(twoAccountConfig(), dir, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg2.Close() }()
	ac, _ := reg2.accessOf("lab_smith")
	if len(ac.AllowUsers) != 1 || ac.AllowUsers[0] != "alice" {
		t.Errorf("access not persisted: %+v", ac)
	}
}

// TestRegistryConcurrentCreateResolve hammers create + resolve concurrently to
// prove the RWMutex discipline under -race.
func TestRegistryConcurrentCreateResolve(t *testing.T) {
	reg, err := NewRegistry(twoAccountConfig(), t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = reg.Create(AccountConfig{Name: "acct" + strconv.Itoa(i), Balance: 100, Rate: 1, Window: "1000000s"})
		}(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = reg.Resolve("lab_smith")
			_ = reg.Names()
		}()
	}
	wg.Wait()
	if len(reg.Names()) != 22 { // 2 config + 20 created
		t.Errorf("account count = %d, want 22", len(reg.Names()))
	}
}
