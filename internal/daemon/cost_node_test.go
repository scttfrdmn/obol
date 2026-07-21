package daemon

import (
	"testing"

	"github.com/scttfrdmn/obol/internal/wire"
)

// TestNodeRatePerSecond covers the h/m/s → integer units/second normalization,
// including the divisibility constraint that protects the kernel's integer money.
func TestNodeRatePerSecond(t *testing.T) {
	ok := []struct {
		nr   NodeRate
		want int64
	}{
		{NodeRate{Rate: 10}, 10},            // default "s"
		{NodeRate{Rate: 10, Per: "s"}, 10},  // per second
		{NodeRate{Rate: 120, Per: "m"}, 2},  // 120/min = 2/s
		{NodeRate{Rate: 3600, Per: "h"}, 1}, // 3600/hr = 1/s
		{NodeRate{Rate: 7200, Per: "h"}, 2}, // 7200/hr = 2/s
	}
	for _, c := range ok {
		got, err := c.nr.perSecond()
		if err != nil {
			t.Errorf("%+v: unexpected error %v", c.nr, err)
		} else if got != c.want {
			t.Errorf("%+v perSecond = %d, want %d", c.nr, got, c.want)
		}
	}
	bad := []NodeRate{
		{Rate: 250, Per: "h"}, // 250/3600 not whole
		{Rate: 10, Per: "m"},  // 10/60 not whole
		{Rate: 0},             // non-positive
		{Rate: -5},            // negative
		{Rate: 10, Per: "d"},  // bad unit
	}
	for _, nr := range bad {
		if _, err := nr.perSecond(); err == nil {
			t.Errorf("%+v: expected error, got none", nr)
		}
	}
}

// TestNodeCostWorstAndRate covers worst-case selection and per-node lookup.
func TestNodeCostWorstAndRate(t *testing.T) {
	cfg := &Config{
		Accounts: []AccountConfig{{Name: "a", Balance: 1, Rate: 1}},
		NodeTypes: map[string]NodeRate{
			"spr":  {Rate: 10},
			"icx":  {Rate: 6},
			"h100": {Rate: 36000, Per: "h"}, // 10/s
		},
		Partitions: []PartitionConfig{
			{Name: "mixed", NodeTypes: []string{"spr", "icx"}},
			{Name: "gpu", NodeTypes: []string{"h100"}},
		},
	}
	nc, err := BuildNodeCost(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !nc.enabled() {
		t.Fatal("node cost should be enabled")
	}
	if w := nc.worstRate("mixed"); w != 10 { // max(spr=10, icx=6)
		t.Errorf("worstRate(mixed) = %d, want 10", w)
	}
	if w := nc.worstRate("gpu"); w != 10 { // h100 normalized to 10/s
		t.Errorf("worstRate(gpu) = %d, want 10", w)
	}
	if w := nc.worstRate("unconfigured"); w != 0 {
		t.Errorf("worstRate(unconfigured) = %d, want 0 (fallback)", w)
	}
	if r := nc.rate("icx"); r != 6 {
		t.Errorf("rate(icx) = %d, want 6", r)
	}
	if r := nc.rate("unknown"); r != 0 {
		t.Errorf("rate(unknown) = %d, want 0", r)
	}
}

// TestNilNodeCostSafe confirms the nil resolver is safe (fallback path).
func TestNilNodeCostSafe(t *testing.T) {
	var nc *NodeCost
	if nc.enabled() || nc.worstRate("x") != 0 || nc.rate("x") != 0 {
		t.Error("nil NodeCost should be inert")
	}
}

// TestGateEscrowsWorstCase confirms the gate escrows a partition's worst-case
// node rate when node-type pricing is configured, ignoring TRES/flat.
func TestGateEscrowsWorstCase(t *testing.T) {
	cfg := &Config{
		Accounts:   []AccountConfig{{Name: "lab", Balance: 1_000_000, Rate: 1, Window: "1000000s"}},
		NodeTypes:  map[string]NodeRate{"spr": {Rate: 10}, "icx": {Rate: 6}},
		Partitions: []PartitionConfig{{Name: "mixed", NodeTypes: []string{"spr", "icx"}}},
	}
	reg, err := NewRegistry(cfg, t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	nc, _ := BuildNodeCost(cfg)
	srv := NewWithRegistry(reg, testNow, Weights{})
	srv.SetNodeCost(nc)

	// 100s job in "mixed" -> worst rate 10 -> escrow 1000 (not the flat rate 1).
	resp := srv.handleGate(&wire.GateRequest{Account: "lab", Partition: "mixed", TimeLimit: 100, NTasks: 1})
	if resp.GateResp == nil || !resp.GateResp.Allow {
		t.Fatalf("gate rejected: %+v", resp.GateResp)
	}
	lab, _ := reg.Resolve("lab")
	if lab.Balance() != 1_000_000-1000 {
		t.Errorf("balance = %d, want %d (worst-case 10/s * 100s)", lab.Balance(), 1_000_000-1000)
	}
}

// TestBindReprices confirms the full node-type flow at the daemon: gate escrows
// worst-case, BIND with the actual (cheaper) node type reprices the escrow down
// before Start, and settle bills at the trued-up rate.
func TestBindReprices(t *testing.T) {
	cfg := &Config{
		Accounts:   []AccountConfig{{Name: "lab", Balance: 1_000_000, Rate: 1, Window: "1000000s"}},
		NodeTypes:  map[string]NodeRate{"spr": {Rate: 10}, "icx": {Rate: 6}},
		Partitions: []PartitionConfig{{Name: "mixed", NodeTypes: []string{"spr", "icx"}}},
	}
	reg, err := NewRegistry(cfg, t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	nc, _ := BuildNodeCost(cfg)
	srv := NewWithRegistry(reg, testNow, Weights{})
	srv.SetNodeCost(nc)
	lab, _ := reg.Resolve("lab")

	// Gate: 100s in "mixed" -> worst rate 10 -> escrow 1000.
	g := srv.handleGate(&wire.GateRequest{Account: "lab", Partition: "mixed", TimeLimit: 100, NTasks: 1})
	if g.GateResp == nil || !g.GateResp.Allow {
		t.Fatalf("gate rejected: %+v", g.GateResp)
	}
	token := g.GateResp.Token
	if lab.Balance() != 1_000_000-1000 {
		t.Fatalf("after gate balance = %d, want %d (worst-case)", lab.Balance(), 1_000_000-1000)
	}

	// BIND with the actual node type icx (rate 6) -> reprice to 6*100=600,
	// refunding 400.
	b := srv.handleBind(&wire.BindRequest{Token: token, JobID: "1", NodeType: "icx"})
	if b.BindResp == nil || !b.BindResp.OK {
		t.Fatalf("bind failed: %+v", b.BindResp)
	}
	if lab.Balance() != 1_000_000-600 {
		t.Errorf("after bind-reprice balance = %d, want %d (icx rate)", lab.Balance(), 1_000_000-600)
	}

	// Settle full runtime -> billed at the repriced rate 6*100=600.
	s := srv.handleSettle(&wire.SettleRequest{JobID: "1", Kind: wire.SettleComplete, Runtime: 100})
	if s.SettleResp == nil || !s.SettleResp.OK {
		t.Fatalf("settle failed: %+v", s.SettleResp)
	}
	if r := lab.Report(testNow()); r.Consumed != 600 {
		t.Errorf("consumed = %d, want 600 (trued-up rate)", r.Consumed)
	}
	if ok, _ := lab.ConservationOK(); !ok {
		t.Error("conservation broken")
	}
}

// TestBindUnknownNodeTypeKeepsWorstCase confirms an unknown/absent node type at
// BIND leaves the worst-case escrow in place (no reprice).
func TestBindUnknownNodeTypeKeepsWorstCase(t *testing.T) {
	cfg := &Config{
		Accounts:   []AccountConfig{{Name: "lab", Balance: 1_000_000, Rate: 1, Window: "1000000s"}},
		NodeTypes:  map[string]NodeRate{"spr": {Rate: 10}},
		Partitions: []PartitionConfig{{Name: "p", NodeTypes: []string{"spr"}}},
	}
	reg, _ := NewRegistry(cfg, t.TempDir(), false, testNow)
	t.Cleanup(func() { _ = reg.Close() })
	nc, _ := BuildNodeCost(cfg)
	srv := NewWithRegistry(reg, testNow, Weights{})
	srv.SetNodeCost(nc)
	lab, _ := reg.Resolve("lab")

	g := srv.handleGate(&wire.GateRequest{Account: "lab", Partition: "p", TimeLimit: 100, NTasks: 1})
	token := g.GateResp.Token
	// BIND with a node type that has no configured rate -> no reprice.
	srv.handleBind(&wire.BindRequest{Token: token, JobID: "1", NodeType: "mystery"})
	if lab.Balance() != 1_000_000-1000 {
		t.Errorf("balance = %d, want worst-case %d (unknown node kept escrow)", lab.Balance(), 1_000_000-1000)
	}
}
