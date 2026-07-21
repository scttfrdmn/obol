package budget

import (
	"errors"
	"testing"
)

// TestTopUpRaisesBalanceAndB0 confirms top-up adds to BOTH the balance and the
// original allocation B0, so conservation (B0 == B+Reserved+Consumed+WriteOff)
// holds exactly before and after.
func TestTopUpRaisesBalanceAndB0(t *testing.T) {
	bd := New(1, 1000, 0, 100000)

	if ok, _ := bd.ConservationOK(); !ok {
		t.Fatal("fresh budget not conserving")
	}
	if err := bd.TopUp(500, 10); err != nil {
		t.Fatalf("TopUp: %v", err)
	}
	r := bd.Report(10)
	if r.B != 1500 {
		t.Errorf("B after topup = %d, want 1500", r.B)
	}
	if r.B0 != 1500 {
		t.Errorf("B0 after topup = %d, want 1500", r.B0)
	}
	if !r.ConservationOK {
		t.Errorf("conservation broken after topup: sum=%d", r.ConservationSum)
	}
}

// TestTopUpFundsNewWork confirms money added by top-up is spendable: a submit
// that would have failed before the top-up succeeds after it.
func TestTopUpFundsNewWork(t *testing.T) {
	bd := New(1, 100, 0, 100000) // only 100 units

	// A 500-unit job (cost 1*500) can't be funded yet.
	if err := bd.Submit("big", 500, 10); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("expected ErrInsufficient before topup, got %v", err)
	}
	// Top up, then the same job funds.
	if err := bd.TopUp(1000, 10); err != nil {
		t.Fatal(err)
	}
	if err := bd.Submit("big", 500, 10); err != nil {
		t.Fatalf("submit after topup failed: %v", err)
	}
	if bal := bd.Balance(); bal != 1100-500 {
		t.Errorf("balance = %d, want 600", bal)
	}
	if ok, _ := bd.ConservationOK(); !ok {
		t.Error("conservation broken after topup+submit")
	}
}

// TestTopUpOnLapsedBudget confirms top-up works regardless of lifecycle status —
// it's an admin action, not a submit. Useful to refill before reopening.
func TestTopUpOnLapsedBudget(t *testing.T) {
	bd := New(1, 100, 0, 100000)
	bd.Lapse()
	if err := bd.TopUp(400, 10); err != nil {
		t.Fatalf("TopUp on lapsed budget failed: %v", err)
	}
	if bd.Balance() != 500 {
		t.Errorf("balance after topup = %d, want 500", bd.Balance())
	}
}

// TestTopUpRejectsNonPositive confirms add-only: zero/negative is rejected (and
// leaves the ledger untouched).
func TestTopUpRejectsNonPositive(t *testing.T) {
	bd := New(1, 100, 0, 100000)
	for _, amt := range []Units{0, -1, -1000} {
		if err := bd.TopUp(amt, 10); err == nil {
			t.Errorf("TopUp(%d) should be rejected", amt)
		}
	}
	if bd.Balance() != 100 {
		t.Errorf("balance changed by a rejected topup: %d", bd.Balance())
	}
}

// TestTopUpDurableRecovery is the replay-path guard: top up, crash, recover, and
// confirm B and B0 survive — proving the WAL carries the top-up amount.
func TestTopUpDurableRecovery(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 1, 1000, 0, 100000, false)
	if err != nil {
		t.Fatal(err)
	}
	bd.TopUp(2500, 10)
	bd.Submit("j1", 100, 20)
	before := fingerprint(bd)
	bd.Close()

	rec, err := OpenBudget(dir, false)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	defer func() { _ = rec.Close() }()
	if after := fingerprint(rec); before != after {
		t.Fatalf("state diverged across crash:\n before=%s\n after =%s", before, after)
	}
	r := rec.Report(20)
	if r.B0 != 3500 { // 1000 + 2500
		t.Errorf("recovered B0 = %d, want 3500 (WAL did not carry topup)", r.B0)
	}
	if ok, _ := rec.ConservationOK(); !ok {
		t.Error("recovered conservation broken")
	}
}
