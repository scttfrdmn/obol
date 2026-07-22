package daemon

import (
	"testing"

	"github.com/scttfrdmn/obol/internal/wire"
)

// arrayServer builds a single-account daemon (flat rate 1) for array-task tests.
func arrayServer(t *testing.T) *Server {
	t.Helper()
	cfg := &Config{Accounts: []AccountConfig{
		{Name: "lab", Balance: 100000, Rate: 1, Window: "1000000s"},
	}}
	reg, err := NewRegistry(cfg, t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return NewWithRegistry(reg, testNow, Weights{})
}

// TestArrayGateBindSettlePerTask drives the full per-task array path (#103): gate
// N tasks (one escrow), bind + settle each task by index, and assert each task's
// slice moves independently and the budget conserves throughout.
func TestArrayGateBindSettlePerTask(t *testing.T) {
	srv := arrayServer(t)
	lab, _ := srv.reg.Resolve("lab")

	// Array of 4 tasks, rate 1, walltime 100 => each slice 100, whole array 400.
	g := srv.handleGate(&wire.GateRequest{Account: "lab", Partition: "cloud", TimeLimit: 100, NTasks: 4})
	if g.GateResp == nil || !g.GateResp.Allow {
		t.Fatalf("array gate rejected: %+v", g.GateResp)
	}
	tok := g.GateResp.Token
	if lab.Balance() != 100000-400 {
		t.Fatalf("after array gate balance = %d, want %d (4*100 escrowed)", lab.Balance(), 100000-400)
	}

	// Bind + settle each task by index. Task 0 completes early (runtime 40 → bills
	// 40, refunds 60); tasks 1-3 run full (bill 100 each).
	for idx := 0; idx < 4; idx++ {
		b := srv.handleBind(&wire.BindRequest{Token: tok, JobID: jobTaskID(idx), IsArrayTask: true, Idx: idx})
		if b.BindResp == nil || !b.BindResp.OK {
			t.Fatalf("bind task %d rejected: %+v", idx, b.BindResp)
		}
	}
	// Settle task 0 early.
	if s := srv.handleSettle(&wire.SettleRequest{JobID: jobTaskID(0), Kind: wire.SettleComplete, Runtime: 40, IsArrayTask: true, Idx: 0}); s.SettleResp == nil || !s.SettleResp.OK {
		t.Fatalf("settle task 0: %+v", s.SettleResp)
	}
	// After task 0: reserved for it (100) released; billed 40, refunded 60.
	// balance = 100000 - 400 + 60 = 99660.
	if lab.Balance() != 99660 {
		t.Errorf("after task 0 balance = %d, want 99660", lab.Balance())
	}
	if ok, _ := lab.ConservationOK(); !ok {
		t.Error("conservation broken after task 0")
	}

	// Settle tasks 1-3 at full runtime (bill 100 each, no refund).
	for idx := 1; idx < 4; idx++ {
		if s := srv.handleSettle(&wire.SettleRequest{JobID: jobTaskID(idx), Kind: wire.SettleComplete, Runtime: 100, IsArrayTask: true, Idx: idx}); s.SettleResp == nil || !s.SettleResp.OK {
			t.Fatalf("settle task %d: %+v", idx, s.SettleResp)
		}
	}
	// Total billed: 40 (task0) + 300 (tasks1-3) = 340. balance = 100000 - 340.
	if lab.Balance() != 100000-340 {
		t.Errorf("final balance = %d, want %d (billed 340)", lab.Balance(), 100000-340)
	}
	if ok, _ := lab.ConservationOK(); !ok {
		t.Error("conservation broken after all tasks")
	}
	if lab.LiveArrays() != 0 {
		t.Errorf("array escrow not closed: LiveArrays = %d", lab.LiveArrays())
	}
	// Routing cleaned up once the last task settled.
	srv.mu.Lock()
	_, budgetLeft := srv.tokenBudget[tok]
	_, arrayNLeft := srv.tokenArrayN[tok]
	srv.mu.Unlock()
	if budgetLeft || arrayNLeft {
		t.Errorf("token routing leaked after final task: budget=%v arrayN=%v", budgetLeft, arrayNLeft)
	}
}

// TestArrayMixedTerminalKinds confirms per-task settle routes each terminal kind
// (complete/timeout/cancel/infrafail) to the matching *Task transition, and the
// array conserves across a mix.
func TestArrayMixedTerminalKinds(t *testing.T) {
	srv := arrayServer(t)
	lab, _ := srv.reg.Resolve("lab")
	g := srv.handleGate(&wire.GateRequest{Account: "lab", Partition: "cloud", TimeLimit: 100, NTasks: 4})
	tok := g.GateResp.Token
	for idx := 0; idx < 4; idx++ {
		srv.handleBind(&wire.BindRequest{Token: tok, JobID: jobTaskID(idx), IsArrayTask: true, Idx: idx})
	}
	// 0 complete(30), 1 timeout, 2 cancel(50), 3 infrafail(20).
	kinds := []struct {
		kind    wire.SettleKind
		runtime int64
		elapsed int64
	}{
		{wire.SettleComplete, 30, 0},
		{wire.SettleTimeout, 0, 0},
		{wire.SettleCancel, 0, 50},
		{wire.SettleInfraFail, 0, 20},
	}
	for idx, k := range kinds {
		s := srv.handleSettle(&wire.SettleRequest{
			JobID: jobTaskID(idx), Kind: k.kind, Runtime: k.runtime, Elapsed: k.elapsed,
			IsArrayTask: true, Idx: idx,
		})
		if s.SettleResp == nil || !s.SettleResp.OK {
			t.Fatalf("settle task %d (%s): %+v", idx, k.kind, s.SettleResp)
		}
	}
	if ok, sum := lab.ConservationOK(); !ok {
		t.Errorf("conservation broken after mixed terminals: sum=%d", sum)
	}
	if lab.LiveArrays() != 0 {
		t.Errorf("array not closed: LiveArrays=%d", lab.LiveArrays())
	}
}

// TestArrayUnfundedRejected confirms an array whose whole cost exceeds the balance
// is rejected at the gate (all-or-nothing), nothing escrowed.
func TestArrayUnfundedRejected(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{Name: "lab", Balance: 250, Rate: 1, Window: "1000000s"}}}
	reg, err := NewRegistry(cfg, t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	srv := NewWithRegistry(reg, testNow, Weights{})
	// 4 tasks * 100 = 400 > 250 balance.
	g := srv.handleGate(&wire.GateRequest{Account: "lab", Partition: "cloud", TimeLimit: 100, NTasks: 4})
	if g.GateResp == nil || g.GateResp.Allow {
		t.Fatalf("over-budget array should be rejected: %+v", g.GateResp)
	}
	lab, _ := reg.Resolve("lab")
	if lab.Balance() != 250 {
		t.Errorf("rejected array reserved money: balance = %d", lab.Balance())
	}
}

// jobTaskID mimics Slurm's per-task job id (<arrayJobId>_<idx>). The daemon only
// uses it as an opaque per-task jobToToken key.
func jobTaskID(idx int) string {
	return "700_" + string(rune('0'+idx))
}
