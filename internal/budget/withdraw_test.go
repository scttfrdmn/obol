package budget

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestWithdrawLowersBalanceAndB0 confirms withdraw removes from BOTH the balance
// and the original allocation B0 (the anchor), so conservation holds exactly
// before and after — the money-symmetric inverse of TopUp.
func TestWithdrawLowersBalanceAndB0(t *testing.T) {
	bd := New(1, 1000, 0, 100000)

	if err := bd.Withdraw(400, 10); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	r := bd.Report(10)
	if r.B != 600 {
		t.Errorf("B after withdraw = %d, want 600", r.B)
	}
	if r.B0 != 600 {
		t.Errorf("B0 after withdraw = %d, want 600", r.B0)
	}
	if !r.ConservationOK {
		t.Errorf("conservation broken after withdraw: sum=%d", r.ConservationSum)
	}
}

// TestWithdrawCapsAtAvailableBalance confirms only available balance moves:
// reserved (and consumed) money is committed to live work and must not be
// withdrawn out from under it. A withdraw exceeding B is rejected, ledger intact.
func TestWithdrawCapsAtAvailableBalance(t *testing.T) {
	bd := New(1, 1000, 0, 100000)
	// Reserve 300 via a live submit; available B is now 700.
	if err := bd.Submit("j1", 300, 10); err != nil {
		t.Fatal(err)
	}
	if bd.Balance() != 700 {
		t.Fatalf("B after submit = %d, want 700", bd.Balance())
	}
	// Withdrawing more than available B (700) is rejected — can't touch the
	// reserved 300.
	if err := bd.Withdraw(800, 10); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("over-withdraw: got %v, want ErrInsufficient", err)
	}
	if bd.Balance() != 700 {
		t.Errorf("balance changed on a rejected withdraw: %d", bd.Balance())
	}
	// Withdrawing exactly the available balance succeeds and leaves the reserved
	// money conserved.
	if err := bd.Withdraw(700, 10); err != nil {
		t.Fatalf("withdraw of full available balance failed: %v", err)
	}
	if bd.Balance() != 0 {
		t.Errorf("B = %d, want 0", bd.Balance())
	}
	if ok, sum := bd.ConservationOK(); !ok {
		t.Errorf("conservation broken: sum=%d B0=%d", sum, bd.Report(10).B0)
	}
}

// TestWithdrawRejectsNonPositive confirms remove-only: zero/negative rejected,
// ledger untouched.
func TestWithdrawRejectsNonPositive(t *testing.T) {
	bd := New(1, 100, 0, 100000)
	for _, amt := range []Units{0, -1, -1000} {
		if err := bd.Withdraw(amt, 10); err == nil {
			t.Errorf("Withdraw(%d) should be rejected", amt)
		}
	}
	if bd.Balance() != 100 {
		t.Errorf("balance changed by a rejected withdraw: %d", bd.Balance())
	}
}

// TestWithdrawOnLapsedBudget confirms withdraw works regardless of lifecycle
// status — sweeping a lapsed budget's leftover before reallocation is the point.
func TestWithdrawOnLapsedBudget(t *testing.T) {
	bd := New(1, 500, 0, 100000)
	bd.Lapse()
	if err := bd.Withdraw(500, 10); err != nil {
		t.Fatalf("Withdraw on lapsed budget failed: %v", err)
	}
	if bd.Balance() != 0 {
		t.Errorf("balance after withdraw = %d, want 0", bd.Balance())
	}
	if ok, _ := bd.ConservationOK(); !ok {
		t.Error("conservation broken after lapsed withdraw")
	}
}

// TestWithdrawDurableRecovery is the replay-path guard: withdraw, crash, recover,
// and confirm B and B0 survive — proving the WAL carries the withdraw amount and
// its Xfer tag.
func TestWithdrawDurableRecovery(t *testing.T) {
	dir := t.TempDir()
	bd, err := NewDurable(dir, 1, 1000, 0, 100000, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.WithdrawXfer(250, "xfer-abc", 10); err != nil {
		t.Fatal(err)
	}
	if err := bd.Submit("j1", 100, 20); err != nil {
		t.Fatal(err)
	}
	before := fingerprint(bd)
	_ = bd.Close()

	rec, err := OpenBudget(dir, false)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	defer func() { _ = rec.Close() }()
	if after := fingerprint(rec); before != after {
		t.Fatalf("state diverged across crash:\n before=%s\n after =%s", before, after)
	}
	if r := rec.Report(20); r.B0 != 750 { // 1000 - 250
		t.Errorf("recovered B0 = %d, want 750 (WAL did not carry withdraw)", r.B0)
	}
	if ok, _ := rec.ConservationOK(); !ok {
		t.Error("recovered conservation broken")
	}

	// The Xfer tag must survive replay so transfer recovery can find the leg.
	has, err := HasXfer(dir, KindWithdraw, "xfer-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("HasXfer did not find the tagged withdraw leg after recovery")
	}
	if other, _ := HasXfer(dir, KindWithdraw, "nope"); other {
		t.Error("HasXfer matched a nonexistent transfer id")
	}
	// A withdraw leg is not a topup leg for the same id.
	if wrongKind, _ := HasXfer(dir, KindTopUp, "xfer-abc"); wrongKind {
		t.Error("HasXfer matched the wrong command kind")
	}
}

// TestWithdrawTopUpConserveUnderRace runs concurrent tagged topups and withdraws
// against one budget and asserts conservation holds throughout (invariant #1,
// under -race). Each op is individually atomic; the net effect is deterministic.
func TestWithdrawTopUpConserveUnderRace(t *testing.T) {
	bd := New(1, 100000, 0, 1_000_000)
	const workers, iters = 8, 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := fmt.Sprintf("x-%d-%d", w, i)
				// Deposit then withdraw the same amount: net zero, but interleaved
				// across workers so the ledger is hammered concurrently.
				_ = bd.TopUpXfer(10, id, Seconds(i))
				_ = bd.WithdrawXfer(10, id, Seconds(i))
			}
		}(w)
	}
	wg.Wait()

	if ok, sum := bd.ConservationOK(); !ok {
		t.Fatalf("conservation broken after concurrent topup/withdraw: sum=%d", sum)
	}
	// Net effect is zero: balance and B0 return to the starting allocation.
	if r := bd.Report(0); r.B != 100000 || r.B0 != 100000 {
		t.Errorf("after balanced churn B=%d B0=%d, want 100000/100000", r.B, r.B0)
	}
}
