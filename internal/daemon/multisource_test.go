package daemon

import (
	"testing"

	"github.com/scttfrdmn/obol/internal/budget"
	"github.com/scttfrdmn/obol/internal/wire"
)

// multiSourceServer builds a daemon over three accounts at a uniform flat rate 1
// so the funding-plan rate is unambiguous. grant is small, startup mid, disc big.
func multiSourceServer(t *testing.T, grant, startup, disc budget.Units) *Server {
	t.Helper()
	cfg := &Config{Accounts: []AccountConfig{
		{Name: "grant", Balance: grant, Rate: 1, Window: "1000000s"},
		{Name: "startup", Balance: startup, Rate: 1, Window: "1000000s"},
		{Name: "disc", Balance: disc, Rate: 1, Window: "1000000s"},
	}}
	reg, err := NewRegistry(cfg, t.TempDir(), false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return NewWithRegistry(reg, testNow, Weights{})
}

// bal is a helper: current balance of an account.
func bal(t *testing.T, srv *Server, acct string) budget.Units {
	t.Helper()
	bd, err := srv.reg.Resolve(acct)
	if err != nil {
		t.Fatal(err)
	}
	return bd.Balance()
}

// allConserve asserts every named account's budget conserves.
func allConserve(t *testing.T, srv *Server, accts ...string) {
	t.Helper()
	for _, a := range accts {
		bd, _ := srv.reg.Resolve(a)
		if ok, sum := bd.ConservationOK(); !ok {
			t.Errorf("%s conservation broken (sum=%d)", a, sum)
		}
	}
}

// TestMultiSourceGateOrderedFallback: a job costing more than the grant drains the
// grant to a whole-second boundary and spills the remainder to startup.
func TestMultiSourceGateOrderedFallback(t *testing.T) {
	// rate 1, walltime 300 → cost 300. grant=100 funds first 100s, startup the 200.
	srv := multiSourceServer(t, 100, 100000, 100000)
	g := srv.handleGate(&wire.GateRequest{
		Sources: []string{"grant", "startup"}, TimeLimit: 300, NTasks: 1,
	})
	if g.GateResp == nil || !g.GateResp.Allow {
		t.Fatalf("multi-source gate rejected: %+v", g.GateResp)
	}
	if bal(t, srv, "grant") != 0 {
		t.Errorf("grant = %d, want 0 (fully drawn)", bal(t, srv, "grant"))
	}
	if bal(t, srv, "startup") != 100000-200 {
		t.Errorf("startup = %d, want %d", bal(t, srv, "startup"), 100000-200)
	}
	allConserve(t, srv, "grant", "startup")
}

// TestMultiSourceGateInsufficient: sources jointly can't cover the job → reject,
// nothing reserved anywhere (all-or-nothing).
func TestMultiSourceGateInsufficient(t *testing.T) {
	// cost 300, sources total 100+150 = 250 < 300 → reject.
	srv := multiSourceServer(t, 100, 150, 0)
	g := srv.handleGate(&wire.GateRequest{
		Sources: []string{"grant", "startup"}, TimeLimit: 300, NTasks: 1,
	})
	if g.GateResp == nil || g.GateResp.Allow {
		t.Fatalf("under-funded multi-source gate should reject: %+v", g.GateResp)
	}
	if bal(t, srv, "grant") != 100 || bal(t, srv, "startup") != 150 {
		t.Errorf("rejected gate reserved money: grant=%d startup=%d", bal(t, srv, "grant"), bal(t, srv, "startup"))
	}
	allConserve(t, srv, "grant", "startup")
}

// TestMultiSourceGateRollback exercises the true rollback path: the plan is
// fundable from the balance snapshot, so leg 1 (grant) is placed — but leg 2's
// SubmitAt then fails (its budget is lapsed), forcing leg 1 to be rolled back
// with a full refund. The gate rejects and grant is left untouched.
func TestMultiSourceGateRollback(t *testing.T) {
	srv := multiSourceServer(t, 100, 100000, 0)
	// Lapse startup: fundingPlan still sees its balance (fundable), but SubmitAt on
	// a lapsed budget returns ErrLapsed → leg 2 fails after leg 1 was placed.
	stbd, _ := srv.reg.Resolve("startup")
	stbd.Lapse()

	g := srv.handleGate(&wire.GateRequest{
		Sources: []string{"grant", "startup"}, TimeLimit: 300, NTasks: 1,
	})
	if g.GateResp == nil || g.GateResp.Allow {
		t.Fatalf("gate should reject when a later leg fails: %+v", g.GateResp)
	}
	// Leg 1 (grant) was placed then rolled back → full refund, balance restored.
	if bal(t, srv, "grant") != 100 {
		t.Errorf("grant not rolled back: balance = %d, want 100", bal(t, srv, "grant"))
	}
	// grant has no live escrow left.
	if gbd, _ := srv.reg.Resolve("grant"); gbd.Live() != 0 {
		t.Errorf("grant has %d live escrows after rollback, want 0", gbd.Live())
	}
	allConserve(t, srv, "grant", "startup")
}

// TestMultiSourceSettleApportionment: at early completion, Σ billed across legs
// equals rate*runtime, the head leg (grant) is fully billed, the tail (startup)
// is refunded, and every budget conserves.
func TestMultiSourceSettleApportionment(t *testing.T) {
	srv := multiSourceServer(t, 100, 100000, 0) // grant funds first 100s
	g := srv.handleGate(&wire.GateRequest{
		Sources: []string{"grant", "startup"}, TimeLimit: 300, NTasks: 1,
	})
	tok := g.GateResp.Token
	srv.handleBind(&wire.BindRequest{Token: tok, JobID: "7"})

	// Job ran 150 of 300s. Grant funded [0,100), startup [100,300). So grant bills
	// 100 (fully consumed), startup bills 50, refunds 150.
	s := srv.handleSettle(&wire.SettleRequest{JobID: "7", Kind: wire.SettleComplete, Runtime: 150})
	if s.SettleResp == nil || !s.SettleResp.OK {
		t.Fatalf("settle failed: %+v", s.SettleResp)
	}
	// grant: started 100 reserved, billed 100 → balance 0.
	if b := bal(t, srv, "grant"); b != 0 {
		t.Errorf("grant balance = %d, want 0 (fully billed its 100s slice)", b)
	}
	// startup: reserved 200, billed 50, refunded 150 → 100000 - 50.
	if b := bal(t, srv, "startup"); b != 100000-50 {
		t.Errorf("startup balance = %d, want %d (billed 50 of its 200s slice)", b, 100000-50)
	}
	allConserve(t, srv, "grant", "startup")
}

// TestMultiSourceSettleFullCompletion: running the whole walltime bills every leg
// its full slice, refunds nothing, and the total billed == cost.
func TestMultiSourceSettleFullCompletion(t *testing.T) {
	srv := multiSourceServer(t, 100, 100000, 0)
	g := srv.handleGate(&wire.GateRequest{
		Sources: []string{"grant", "startup"}, TimeLimit: 300, NTasks: 1,
	})
	tok := g.GateResp.Token
	srv.handleBind(&wire.BindRequest{Token: tok, JobID: "8"})
	srv.handleSettle(&wire.SettleRequest{JobID: "8", Kind: wire.SettleComplete, Runtime: 300})
	// grant billed its full 100, startup its full 200 → grant 0, startup -200.
	if bal(t, srv, "grant") != 0 || bal(t, srv, "startup") != 100000-200 {
		t.Errorf("full completion balances: grant=%d startup=%d", bal(t, srv, "grant"), bal(t, srv, "startup"))
	}
	allConserve(t, srv, "grant", "startup")
}

// TestMultiSourceInfraFailPerBudgetPolicy: a node failure fans out to each leg,
// and each leg applies ITS OWN budget's BillInfraFailures flag — the on-prem
// source writes off its slice, the cloud source bills its slice.
func TestMultiSourceInfraFailMixedPolicy(t *testing.T) {
	srv := multiSourceServer(t, 100, 100000, 0)
	// grant = on-prem (write off infra loss); startup = cloud (bill it).
	gbd, _ := srv.reg.Resolve("grant")
	gbd.BillInfraFailures = false
	sbd, _ := srv.reg.Resolve("startup")
	sbd.BillInfraFailures = true

	g := srv.handleGate(&wire.GateRequest{
		Sources: []string{"grant", "startup"}, TimeLimit: 300, NTasks: 1,
	})
	tok := g.GateResp.Token
	srv.handleBind(&wire.BindRequest{Token: tok, JobID: "9"})

	// Infra fail after 150s elapsed: grant slice [0,100) elapsed 100 (its whole
	// slice), startup slice [100,300) elapsed 50. Both budgets conserve; grant
	// wrote off its used, startup billed its used. We assert conservation and that
	// startup's balance reflects a refund of its unused 150s.
	s := srv.handleSettle(&wire.SettleRequest{JobID: "9", Kind: wire.SettleInfraFail, Elapsed: 150})
	if s.SettleResp == nil || !s.SettleResp.OK {
		t.Fatalf("infrafail settle failed: %+v", s.SettleResp)
	}
	// startup reserved 200, used 50 → refund 150 → balance 100000-50.
	if b := bal(t, srv, "startup"); b != 100000-50 {
		t.Errorf("startup balance = %d, want %d", b, 100000-50)
	}
	allConserve(t, srv, "grant", "startup")

	// Confirm the policy actually diverged: grant wrote off (WriteOff>0, Consumed
	// on grant == 0), startup billed (Consumed>0, WriteOff==0).
	gr := gbd.Report(testNow())
	sr := sbd.Report(testNow())
	if gr.WriteOff == 0 || gr.Consumed != 0 {
		t.Errorf("grant (on-prem) should write off, not bill: %+v", gr)
	}
	if sr.Consumed == 0 || sr.WriteOff != 0 {
		t.Errorf("startup (cloud) should bill, not write off: %+v", sr)
	}
}

// TestMultiSourceBackCompatSingleSource: a single source in the list behaves like
// the single-source path — one leg, whole cost.
func TestMultiSourceSingleElement(t *testing.T) {
	srv := multiSourceServer(t, 100000, 0, 0)
	g := srv.handleGate(&wire.GateRequest{
		Sources: []string{"grant"}, TimeLimit: 100, NTasks: 1,
	})
	if g.GateResp == nil || !g.GateResp.Allow {
		t.Fatalf("single-element multi-source gate rejected: %+v", g.GateResp)
	}
	if bal(t, srv, "grant") != 100000-100 {
		t.Errorf("grant = %d, want %d", bal(t, srv, "grant"), 100000-100)
	}
}

// TestMultiSourceArrayWholeTasks (#96): an array is split across sources by whole
// tasks (ordered fallback). grant funds floor(250/100)=2 tasks, startup the rest.
func TestMultiSourceArrayWholeTasks(t *testing.T) {
	// rate 1, walltime 100 => one task costs 100. grant=250 funds 2 whole tasks
	// (its extra 50 stays unused); startup covers the other 2. Array of 4.
	srv := multiSourceServer(t, 250, 100000, 0)
	g := srv.handleGate(&wire.GateRequest{
		Sources: []string{"grant", "startup"}, TimeLimit: 100, NTasks: 4,
	})
	if g.GateResp == nil || !g.GateResp.Allow {
		t.Fatalf("multi-source array gate rejected: %+v", g.GateResp)
	}
	tok := g.GateResp.Token
	// grant escrows 2 tasks (200 of its 250); startup escrows 2 (200).
	if bal(t, srv, "grant") != 250-200 {
		t.Errorf("grant = %d, want 50 (2 tasks * 100 escrowed, 50 left unused)", bal(t, srv, "grant"))
	}
	if bal(t, srv, "startup") != 100000-200 {
		t.Errorf("startup = %d, want %d", bal(t, srv, "startup"), 100000-200)
	}

	// Bind + settle all 4 tasks by global index. Tasks 0,1 -> grant; 2,3 -> startup.
	for idx := 0; idx < 4; idx++ {
		b := srv.handleBind(&wire.BindRequest{Token: tok, JobID: jobTaskID(idx), IsArrayTask: true, Idx: idx})
		if b.BindResp == nil || !b.BindResp.OK {
			t.Fatalf("bind task %d: %+v", idx, b.BindResp)
		}
	}
	// Task 0 completes early (runtime 40 → grant bills 40, refunds 60); 1-3 full.
	srv.handleSettle(&wire.SettleRequest{JobID: jobTaskID(0), Kind: wire.SettleComplete, Runtime: 40, IsArrayTask: true, Idx: 0})
	for idx := 1; idx < 4; idx++ {
		s := srv.handleSettle(&wire.SettleRequest{JobID: jobTaskID(idx), Kind: wire.SettleComplete, Runtime: 100, IsArrayTask: true, Idx: idx})
		if s.SettleResp == nil || !s.SettleResp.OK {
			t.Fatalf("settle task %d: %+v", idx, s.SettleResp)
		}
	}
	// grant: 2 tasks, billed 40 + 100 = 140, refunded 60 → balance 250-140 = 110.
	if bal(t, srv, "grant") != 110 {
		t.Errorf("grant final = %d, want 110 (billed 140 of 200 escrowed)", bal(t, srv, "grant"))
	}
	// startup: 2 tasks billed full (200) → 100000-200.
	if bal(t, srv, "startup") != 100000-200 {
		t.Errorf("startup final = %d, want %d", bal(t, srv, "startup"), 100000-200)
	}
	allConserve(t, srv, "grant", "startup")
	// Both arrays closed; routing cleaned up.
	srv.mu.Lock()
	_, legsLeft := srv.tokenLegs[tok]
	_, nLeft := srv.tokenArrayN[tok]
	srv.mu.Unlock()
	if legsLeft || nLeft {
		t.Errorf("multi-source array routing leaked: legs=%v n=%v", legsLeft, nLeft)
	}
}

// TestMultiSourceArrayUnfunded: sources jointly can't fund all N tasks → reject,
// nothing escrowed anywhere.
func TestMultiSourceArrayUnfunded(t *testing.T) {
	// task cost 100; grant funds 2, startup funds 1 (150→1 whole task) = 3 < 4.
	srv := multiSourceServer(t, 250, 150, 0)
	g := srv.handleGate(&wire.GateRequest{
		Sources: []string{"grant", "startup"}, TimeLimit: 100, NTasks: 4,
	})
	if g.GateResp == nil || g.GateResp.Allow {
		t.Fatalf("under-funded array should reject: %+v", g.GateResp)
	}
	if bal(t, srv, "grant") != 250 || bal(t, srv, "startup") != 150 {
		t.Errorf("rejected array reserved money: grant=%d startup=%d", bal(t, srv, "grant"), bal(t, srv, "startup"))
	}
}

// TestMultiSourceCrashSelfHeal: a partially-placed gate (legs escrowed, never
// bound) is reclaimed by the unbound-token janitor without a journal — the same
// safety net single-source relies on. Simulate a crash by dropping tokenLegs after
// the gate, then sweeping.
func TestMultiSourceCrashSelfHeal(t *testing.T) {
	srv := multiSourceServer(t, 100, 100000, 0)
	g := srv.handleGate(&wire.GateRequest{
		Sources: []string{"grant", "startup"}, TimeLimit: 300, NTasks: 1,
	})
	tok := g.GateResp.Token
	if bal(t, srv, "grant") != 0 || bal(t, srv, "startup") != 100000-200 {
		t.Fatal("gate did not reserve as expected")
	}
	// Simulate a crash before BIND: the in-memory routing is lost, but the escrows
	// are durable (unstarted). Drop tokenLegs to mimic a restart with lost routing.
	srv.mu.Lock()
	delete(srv.tokenLegs, tok)
	srv.mu.Unlock()

	// The janitor reclaims never-started escrows older than the TTL, per budget.
	// Sweep with now well past submit(=testNow) + ttl.
	swept := srv.reg.SweepUnbound(300, testNow()+1000)
	if swept != 2 {
		t.Fatalf("janitor swept %d legs, want 2", swept)
	}
	if bal(t, srv, "grant") != 100 || bal(t, srv, "startup") != 100000 {
		t.Errorf("legs not fully reclaimed: grant=%d startup=%d", bal(t, srv, "grant"), bal(t, srv, "startup"))
	}
	allConserve(t, srv, "grant", "startup")
}
