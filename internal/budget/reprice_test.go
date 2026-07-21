package budget

import (
	"errors"
	"testing"
)

// TestRepriceLowersRate confirms repricing a live escrow to a cheaper rate
// refunds the difference (Reserved -> B), leaves B0 untouched, and holds
// conservation.
func TestRepriceLowersRate(t *testing.T) {
	bd := New(1, 100000, 0, 100000)

	// Submit worst-case: rate 100, w 50 -> reserve 5000.
	if err := bd.SubmitAt("j", 100, 50, 10); err != nil {
		t.Fatal(err)
	}
	if bd.Balance() != 100000-5000 {
		t.Fatalf("after submit balance = %d, want 95000", bd.Balance())
	}
	// Reprice to the actual node's rate 40 -> reserve should become 40*50=2000,
	// refunding 3000.
	if err := bd.Reprice("j", 40, 20); err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if bd.Balance() != 100000-2000 {
		t.Errorf("after reprice balance = %d, want 98000", bd.Balance())
	}
	if ok, sum := bd.ConservationOK(); !ok {
		t.Errorf("conservation broken: %d", sum)
	}
	// Settle bills at the NEW rate: runtime 50 (full) -> used 40*50=2000, no refund.
	if err := bd.Complete("j", 50, 100); err != nil {
		t.Fatal(err)
	}
	if r := bd.Report(100); r.Consumed != 2000 {
		t.Errorf("consumed = %d, want 2000 (billed at repriced rate)", r.Consumed)
	}
	if ok, _ := bd.ConservationOK(); !ok {
		t.Error("conservation broken after settle")
	}
}

// TestRepriceRejectsRaise confirms a reprice may only LOWER the rate — a raise
// would risk overdraft against a worst-case escrow and is rejected.
func TestRepriceRejectsRaise(t *testing.T) {
	bd := New(1, 100000, 0, 100000)
	bd.SubmitAt("j", 10, 50, 10) // reserve 500
	if err := bd.Reprice("j", 20, 20); !errors.Is(err, ErrBadState) {
		t.Errorf("reprice raise should be ErrBadState, got %v", err)
	}
	if bd.Balance() != 100000-500 {
		t.Errorf("balance changed on a rejected reprice: %d", bd.Balance())
	}
}

// TestRepriceRejectsNonPositiveAndUnknown covers the guards.
func TestRepriceRejectsNonPositiveAndUnknown(t *testing.T) {
	bd := New(1, 100000, 0, 100000)
	bd.SubmitAt("j", 10, 50, 10)
	if err := bd.Reprice("j", 0, 20); err == nil {
		t.Error("reprice to 0 should be rejected")
	}
	if err := bd.Reprice("ghost", 5, 20); !errors.Is(err, ErrNoSuchJob) {
		t.Errorf("reprice unknown job = %v, want ErrNoSuchJob", err)
	}
}

// TestRepriceRejectsAfterStart confirms repricing a started escrow is rejected
// (its rate is already committed into rLive/burst). BIND reprices before Start,
// so this never happens in practice, but the guard protects the invariant.
func TestRepriceRejectsAfterStart(t *testing.T) {
	bd := New(1, 100000, 0, 100000)
	bd.SubmitAt("j", 10, 50, 10)
	bd.Start("j", 15)
	if err := bd.Reprice("j", 5, 20); !errors.Is(err, ErrBadState) {
		t.Errorf("reprice after start = %v, want ErrBadState", err)
	}
}

// TestRepriceDurableRecovery is the replay guard: submit worst-case, reprice
// down, crash, recover, and confirm the repriced rate/balance survive.
func TestRepriceDurableRecovery(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 1, 100000, 0, 100000, false)
	if err != nil {
		t.Fatal(err)
	}
	bd.SubmitAt("j", 100, 50, 10) // reserve 5000
	bd.Reprice("j", 30, 20)       // -> reserve 1500, refund 3500
	before := fingerprint(bd)
	balBefore := bd.Balance()
	bd.Close()

	rec, err := OpenBudget(dir, false)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	defer func() { _ = rec.Close() }()
	if after := fingerprint(rec); before != after {
		t.Fatalf("state diverged across crash:\n before=%s\n after =%s", before, after)
	}
	if rec.Balance() != balBefore {
		t.Errorf("recovered balance %d != %d (WAL did not carry the repriced rate)", rec.Balance(), balBefore)
	}
	// The recovered escrow settles at the repriced rate.
	if err := rec.Complete("j", 50, 100); err != nil {
		t.Fatal(err)
	}
	if r := rec.Report(100); r.Consumed != 1500 { // 30*50
		t.Errorf("consumed after recovery+settle = %d, want 1500", r.Consumed)
	}
}
