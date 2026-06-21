package budget

import (
	"errors"
	"testing"
)

// c=10/sec, B0=10000, window [0,1000) -> r0 = 10/sec. One job burns at C=10,
// which is exactly r0: a single job never bursts; concurrency is what bursts.
// Burst is now reserved at dispatch (Start), not submit.
func freshBurst() *Budget {
	bd := New(10, 10000, 0, 1000)
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 1.0 // bank up to full budget
	return bd
}

func TestBurstBanksWhenIdle(t *testing.T) {
	bd := freshBurst()
	pot, ceil, _ := bd.BurstSnapshot(300) // idle 300s -> bank r0*300 = 3000
	if pot != 3000 {
		t.Fatalf("banked=%d want 3000 (ceil=%d)", pot, ceil)
	}
	mustConserve(t, bd)
}

func TestBurstCeilingClampsBanking(t *testing.T) {
	bd := New(10, 10000, 0, 1000)
	bd.BurstEnabled = true
	bd.BurstCeilingPct = 0.2 // ceiling = 2000
	pot, ceil, _ := bd.BurstSnapshot(500)
	if pot != 2000 || ceil != 2000 {
		t.Fatalf("pot=%d ceil=%d want 2000/2000", pot, ceil)
	}
}

func TestSingleJobNeverBursts(t *testing.T) {
	bd := freshBurst()
	bd.BurstSnapshot(300) // bank 3000
	bd.Submit("j1", 50, 300)
	// Dispatch at t=300: one job at C=10 == r0 -> excess 0, reserves no burst.
	if err := bd.Start("j1", 300); err != nil {
		t.Fatal(err)
	}
	pot, _, rl := bd.BurstSnapshot(300)
	if pot != 3000 {
		t.Fatalf("single job drew burst: pot=%d want 3000", pot)
	}
	if rl != 10 {
		t.Fatalf("rLive=%d want 10", rl)
	}
	mustConserve(t, bd)
}

func TestConcurrencyBurstsAndDrawsBank(t *testing.T) {
	bd := freshBurst()
	bd.BurstSnapshot(300) // bank 3000
	bd.Submit("A", 50, 300)
	bd.Submit("B", 50, 300)
	// Dispatch A: aggregate 0->10 (==r0), excess 0.
	if err := bd.Start("A", 300); err != nil {
		t.Fatal(err)
	}
	// Dispatch B: aggregate 10->20, excess rate = 20 - max(10,10) = 10.
	// burst reserve = 10 * 50 = 500. Banked 3000 covers it.
	if err := bd.Start("B", 300); err != nil {
		t.Fatal(err)
	}
	pot, _, rl := bd.BurstSnapshot(300)
	if pot != 2500 {
		t.Fatalf("after burst pot=%d want 2500 (3000-500)", pot)
	}
	if rl != 20 {
		t.Fatalf("rLive=%d want 20", rl)
	}
	mustConserve(t, bd)
	if !bd.BurstBoundsOK() {
		t.Fatal("burst bounds violated")
	}
}

func TestBurstRefundsUnusedTail(t *testing.T) {
	bd := freshBurst()
	bd.BurstSnapshot(300)
	bd.Submit("A", 50, 300)
	bd.Submit("B", 50, 300)
	bd.Start("A", 300)
	bd.Start("B", 300) // reserves 500 burst, rLive=20
	// B completes after 20 of 50s. Unused tail = 30/50 of 500 = 300 refunded.
	if err := bd.Complete("B", 20, 300); err != nil {
		t.Fatal(err)
	}
	pot, _, rl := bd.BurstSnapshot(300)
	if pot != 2800 { // 3000 - 500 reserve + 300 refund
		t.Fatalf("pot=%d want 2800", pot)
	}
	if rl != 10 {
		t.Fatalf("rLive=%d want 10", rl)
	}
	mustConserve(t, bd)
}

func TestBurstInsufficientBlocksDispatch(t *testing.T) {
	bd := freshBurst()
	bd.BurstSnapshot(10) // bank only 100 tokens (idle 10s)
	bd.Submit("A", 10, 10)
	bd.Start("A", 10) // sustainable (rLive 0->10), no burst
	bd.Submit("B", 50, 10)
	// Dispatching B needs excess 10 * 50 = 500 > 100 banked. Dispatch BLOCKED;
	// B's money is still escrowed (it waits), nothing corrupted.
	bBefore := bd.Balance()
	if err := bd.Start("B", 10); !errors.Is(err, ErrBurstInsuff) {
		t.Fatalf("want ErrBurstInsuff got %v", err)
	}
	if bd.Balance() != bBefore {
		t.Fatalf("blocked dispatch moved money: %d -> %d", bBefore, bd.Balance())
	}
	pot, _, rl := bd.BurstSnapshot(10)
	if pot != 100 || rl != 10 {
		t.Fatalf("blocked dispatch mutated burst: pot=%d rLive=%d want 100/10", pot, rl)
	}
	// Bank more by idling, then B can dispatch: at t=60, bank = 100 + r0*50 = 600.
	// (rLive=10 from A means net accrual is (10-10)=0... so idling with A running
	// does NOT bank. Cancel A first, then idle.)
	bd.Cancel("A", 5, 10) // A done, rLive back to 0
	bd.BurstSnapshot(70)  // idle 60s from t=10 -> bank ~600
	if err := bd.Start("B", 70); err != nil {
		t.Fatalf("after banking, B should dispatch: %v", err)
	}
	mustConserve(t, bd)
}

func TestBurstDrawCapBlocksDispatch(t *testing.T) {
	bd := freshBurst()
	bd.BurstDrawCap = 200 // a single task may reserve at most 200 tokens
	bd.BurstSnapshot(500) // bank 5000
	bd.Submit("A", 100, 500)
	bd.Start("A", 500) // rLive=10
	bd.Submit("B", 100, 500)
	// B excess 10 * 100 = 1000 > 200 cap -> dispatch blocked regardless of bank.
	if err := bd.Start("B", 500); !errors.Is(err, ErrBurstDrawCap) {
		t.Fatalf("want ErrBurstDrawCap got %v", err)
	}
	// A shorter task: excess 10 * 20 = 200 == cap -> fits.
	bd.Submit("B2", 20, 500)
	if err := bd.Start("B2", 500); err != nil {
		t.Fatalf("w=20 should fit draw cap: %v", err)
	}
	mustConserve(t, bd)
}

// Non-dividing budget: B0=10000 over T=3000 -> r0 = 3.333.../sec. Fixed-point
// accrual must bank the true amount with no per-step drift.
func TestBurstFixedPointNoDrift(t *testing.T) {
	mk := func() *Budget {
		b := New(3, 10000, 0, 3000)
		b.BurstEnabled = true
		b.BurstCeilingPct = 1.0
		return b
	}
	if pot, _, _ := mk().BurstSnapshot(3000); pot != 10000 {
		t.Fatalf("full-window banked=%d want 10000 (truncation gives ~9000)", pot)
	}
	if pot, _, _ := mk().BurstSnapshot(1500); pot != 5000 {
		t.Fatalf("mid-window banked=%d want 5000", pot)
	}
	stepwise := mk()
	for tnow := Seconds(1); tnow <= 3000; tnow++ {
		stepwise.BurstSnapshot(tnow)
	}
	if pot, _, _ := stepwise.BurstSnapshot(3000); pot != 10000 {
		t.Fatalf("stepwise banked=%d want 10000 (no per-step drift)", pot)
	}
}
